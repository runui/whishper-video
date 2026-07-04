package models

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

const defaultLLMChunkSize = 10

const defaultLLMConcurrency = 3

const defaultLLMContextLimit = 32768

const llmPromptSafetyRatio = 0.7

type LLMTranslationRequest struct {
	TargetLanguage    string `json:"targetLanguage"`
	Model             string `json:"model"`
	ChunkSize         int    `json:"chunkSize"`
	Context           string `json:"context"`
	AutoContext       string `json:"-"` // NFO context, filled by backend, not from frontend
	ReferenceExamples string `json:"-"` // reference translations, filled by backend
}

type TranslationProgressCallback func(total, completed, currentSegment int, status string)

type llmSegmentResponse struct {
	Text string `json:"text"`
}

type llmChunkHistory struct {
	segments     []Segment
	translations []llmSegmentResponse
}

type llmChunkDef struct {
	index int
	start int
	end   int
}

type llmTranslator struct {
	endpoint   string
	apiKey     string
	model      string
	timeout    time.Duration
	httpClient *http.Client
	useTools   bool
	toolsTried bool
	maxRetries int
	debugIO    bool
}

type llmRateLimitGate struct {
	mu         sync.Mutex
	until      time.Time
	cooldown   time.Duration
	lastTripAt time.Time
}

var sharedLLMRateLimitGate = &llmRateLimitGate{cooldown: time.Second}

type llmMemoryTranslationCache struct {
	mu    sync.Mutex
	items map[string]string
	order []string
	max   int
}

var sharedLLMTranslationCache = &llmMemoryTranslationCache{items: map[string]string{}, max: 10000}

func newLLMTranslator(modelOverride string) (*llmTranslator, error) {
	endpoint := strings.TrimSpace(os.Getenv("LLM_TRANSLATION_ENDPOINT"))
	if endpoint == "" {
		return nil, fmt.Errorf("LLM_TRANSLATION_ENDPOINT is not configured")
	}

	model := strings.TrimSpace(modelOverride)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("LLM_TRANSLATION_MODEL"))
	}
	if model == "" {
		return nil, fmt.Errorf("LLM_TRANSLATION_MODEL is not configured")
	}

	timeoutSeconds := envInt("LLM_TRANSLATION_TIMEOUT_SECONDS", 120)
	timeout := time.Duration(timeoutSeconds) * time.Second

	useTools := strings.TrimSpace(os.Getenv("LLM_TRANSLATION_NO_TOOLS")) != "true"
	maxRetries := envInt("LLM_TRANSLATION_MAX_RETRIES", 5)
	if maxRetries < 0 {
		maxRetries = 0
	}
	debugIO := strings.TrimSpace(os.Getenv("LLM_TRANSLATION_DEBUG_IO")) == "true"

	return &llmTranslator{
		endpoint:   endpoint,
		apiKey:     strings.TrimSpace(os.Getenv("LLM_TRANSLATION_API_KEY")),
		model:      model,
		timeout:    timeout,
		httpClient: &http.Client{Timeout: timeout},
		useTools:   useTools,
		maxRetries: maxRetries,
		debugIO:    debugIO,
	}, nil
}

func TranslateWhisperResultWithLLM(ctx context.Context, result WhisperResult, source string, req LLMTranslationRequest, onProgress TranslationProgressCallback) (Translation, error) {
	if req.TargetLanguage == "" {
		return Translation{}, fmt.Errorf("targetLanguage is required")
	}

	translator, err := newLLMTranslator(req.Model)
	if err != nil {
		return Translation{}, err
	}

	combinedContext := buildCombinedContext(req.Context, req.AutoContext, req.ReferenceExamples)

	maxConcurrent := envInt("LLM_TRANSLATION_CONCURRENCY", defaultLLMConcurrency)
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}

	log.Debug().Int("totalSegments", len(result.Segments)).Str("targetLang", req.TargetLanguage).Int("concurrency", maxConcurrent).Msg("LLM translation started")

	if source == "" || source == "auto" {
		source = result.Language
	}

	translatedSegments := make([]Segment, len(result.Segments))
	copy(translatedSegments, result.Segments)

	if len(result.Segments) == 0 {
		return Translation{
			SourceLanguage: source,
			TargetLanguage: req.TargetLanguage,
			Status:         TranscriptionStatusDone,
			Engine:         "llm",
			Result: WhisperResult{
				Language: req.TargetLanguage,
				Duration: result.Duration,
				Segments: translatedSegments,
				Text:     "",
			},
		}, nil
	}

	chunkSize := req.ChunkSize
	if chunkSize <= 0 {
		chunkSize = envInt("LLM_TRANSLATION_CHUNK_SIZE", 0)
	}

	if chunkSize > 0 {
		chunkSize = translator.adaptiveChunkSize(result.Segments, chunkSize)
		return translateChunked(ctx, translator, result.Segments, source, req.TargetLanguage, combinedContext, translatedSegments, chunkSize, maxConcurrent, result.Duration, onProgress)
	}

	return translatePerSegment(ctx, translator, result.Segments, source, req.TargetLanguage, combinedContext, translatedSegments, maxConcurrent, result.Duration, onProgress)
}

func translatePerSegment(ctx context.Context, translator *llmTranslator, segments []Segment, source, target, extraContext string, translatedSegments []Segment, maxConcurrent int, duration float64, onProgress TranslationProgressCallback) (Translation, error) {
	type segResult struct {
		index int
		text  string
		err   error
	}

	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	results := make([]segResult, len(segments))
	var completed int32

	for i := range segments {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Error().Int("segment", segments[idx].Index()).Interface("panic", r).Msg("segment translation panicked")
					results[idx] = segResult{index: idx, err: fmt.Errorf("panic: %v", r)}
					atomic.AddInt32(&completed, 1)
				}
			}()

			select {
			case <-ctx.Done():
				results[idx] = segResult{index: idx, err: ctx.Err()}
				atomic.AddInt32(&completed, 1)
				if onProgress != nil {
					onProgress(len(segments), int(atomic.LoadInt32(&completed)), segments[idx].Index(), "cancelled")
				}
				return
			default:
			}

			log.Debug().Int("segment", segments[idx].Index()).Str("text", excerpt(segments[idx].Text, 80)).Msg("LLM translating segment")
			start := time.Now()
			text, err := translator.translateSegment(ctx, segments[idx], source, target, extraContext)
			status := "done"
			if err != nil {
				log.Warn().Err(err).Int("segment", segments[idx].Index()).Str("text", excerpt(segments[idx].Text, 100)).Str("model", translator.model).Dur("duration", time.Since(start)).Msg("LLM segment translation failed")
				results[idx] = segResult{index: idx, err: err}
				status = "error"
			} else {
				log.Debug().Int("segment", segments[idx].Index()).Dur("duration", time.Since(start)).Str("result", excerpt(text, 80)).Msg("LLM segment translated")
				results[idx] = segResult{index: idx, text: text}
			}
			atomic.AddInt32(&completed, 1)
			if onProgress != nil {
				onProgress(len(segments), int(atomic.LoadInt32(&completed)), segments[idx].Index(), status)
			}
		}(i)
	}

	wg.Wait()

	var firstErr error
	var errs []string
	for i, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			errs = append(errs, fmt.Sprintf("seg %d: %s", segments[i].Index(), r.err))
			translatedSegments[i].Text = fmt.Sprintf("[ERROR:%s]", r.err)
		} else {
			translatedSegments[i].Text = r.text
			translatedSegments[i].Words = []Word{}
		}
	}

	var translatedTexts []string
	for _, seg := range translatedSegments {
		if seg.Text != "" {
			translatedTexts = append(translatedTexts, seg.Text)
		}
	}

	translation := Translation{
		SourceLanguage: source,
		TargetLanguage: target,
		Status:         TranscriptionStatusDone,
		Engine:         "llm",
		Result: WhisperResult{
			Language: target,
			Duration: duration,
			Segments: translatedSegments,
			Text:     strings.Join(translatedTexts, "\n"),
		},
	}

	if firstErr != nil {
		return translation, fmt.Errorf("%d/%d segments failed: %s", len(errs), len(segments), strings.Join(errs, "; "))
	}

	return translation, nil
}

func translateChunked(ctx context.Context, translator *llmTranslator, segments []Segment, source, target, extraContext string, translatedSegments []Segment, chunkSize, maxConcurrent int, duration float64, onProgress TranslationProgressCallback) (Translation, error) {
	totalChunks := (len(segments) + chunkSize - 1) / chunkSize
	log.Debug().Int("totalSegments", len(segments)).Int("chunkSize", chunkSize).Int("totalChunks", totalChunks).Msg("LLM translation chunked")

	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	chunks := make([]llmChunkDef, totalChunks)
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(segments) {
			end = len(segments)
		}
		chunks[i] = llmChunkDef{index: i, start: start, end: end}
	}

	historyChunks := envInt("LLM_TRANSLATION_SESSION_HISTORY_CHUNKS", 0)
	if historyChunks > 0 {
		return translateChunkedWithSessionHistory(ctx, translator, segments, chunks, source, target, extraContext, translatedSegments, historyChunks, totalChunks, duration, onProgress)
	}

	type chunkResult struct {
		index int
		items []llmSegmentResponse
		err   error
	}

	var mu sync.Mutex
	var firstErr error
	var cancelReason error
	var errs []string
	var completedSegments int32
	var failedSegments int32
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup

	for _, c := range chunks {
		wg.Add(1)
		sem <- struct{}{}
		go func(c llmChunkDef) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Error().Int("chunk", c.index+1).Interface("panic", r).Msg("chunk translation panicked")
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("panic in chunk %d: %v", c.index+1, r)
					}
					mu.Unlock()
					atomic.AddInt32(&completedSegments, int32(c.end-c.start))
				}
			}()

			select {
			case <-ctx.Done():
				mu.Lock()
				reason := cancelReason
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				cancelErr := fmt.Sprintf("chunk %d cancelled before processing", c.index+1)
				if reason != nil {
					cancelErr = fmt.Sprintf("chunk %d cancelled: %s", c.index+1, reason)
				}
				errs = append(errs, fmt.Sprintf("chunk %d (segments %d-%d): %s", c.index+1, c.start+1, c.end, cancelErr))
				mu.Unlock()
				log.Warn().Err(ctx.Err()).Int("chunk", c.index+1).Str("segments", fmt.Sprintf("%d-%d", c.start+1, c.end)).Str("reason", cancelErr).Msg("LLM chunk translation cancelled")
				atomic.AddInt32(&completedSegments, int32(c.end-c.start))
				if onProgress != nil {
					onProgress(len(segments), int(atomic.LoadInt32(&completedSegments)), c.start+1, "cancelled")
				}
				return
			default:
			}

			// Stagger requests to avoid rate limiting
			if c.index%maxConcurrent != 0 {
				time.Sleep(time.Duration(c.index%maxConcurrent) * 500 * time.Millisecond)
			}

			chunkSegs := segments[c.start:c.end]

			log.Info().Int("chunk", c.index+1).Int("segmentStart", c.start+1).Int("segmentEnd", c.end).Msg("LLM translating chunk")
			items, chunkErr := translator.translateChunk(ctx, chunkSegs, source, target, extraContext)
			if chunkErr != nil && errors.Is(chunkErr, context.Canceled) {
				mu.Lock()
				if cancelReason != nil {
					chunkErr = fmt.Errorf("chunk cancelled: %w", cancelReason)
				}
				mu.Unlock()
			}
			if chunkErr == nil && translator.useTools {
				// Validate tool results — check if all texts are empty
				emptyCount := 0
				for _, item := range items {
					if strings.TrimSpace(item.Text) == "" {
						emptyCount++
					}
				}
				if emptyCount == len(items) {
					// All empty — tools likely returning garbage, disable and retry with per-segment
					translator.useTools = false
					log.Info().Int("chunk", c.index+1).Msg("LLM tools returned all empty, retrying per-segment")
					items, chunkErr = translator.translateChunk(ctx, chunkSegs, source, target, extraContext)
				}
			}
			if chunkErr != nil {
				log.Warn().Err(chunkErr).Int("chunk", c.index+1).Str("endpoint", translator.endpoint).Str("model", translator.model).Str("segments", fmt.Sprintf("%d-%d", c.start+1, c.end)).Msg("LLM chunk translation failed")
				mu.Lock()
				if firstErr == nil {
					firstErr = chunkErr
				}
				errs = append(errs, fmt.Sprintf("chunk %d (segments %d-%d): %s", c.index+1, c.start+1, c.end, chunkErr))
				mu.Unlock()
				failed := int32(c.end - c.start)
				atomic.AddInt32(&completedSegments, failed)
				if atomic.AddInt32(&failedSegments, failed) >= 60 {
					mu.Lock()
					if cancelReason == nil {
						cancelReason = chunkErr
					}
					mu.Unlock()
					cancelFunc()
				}
				if onProgress != nil {
					onProgress(len(segments), int(atomic.LoadInt32(&completedSegments)), c.start+1, "error")
				}
				return
			}

			for i, item := range items {
				if i >= c.end-c.start {
					break
				}
				segmentIndex := c.start + i
				translatedSegments[segmentIndex].Text = strings.TrimSpace(item.Text)
				translatedSegments[segmentIndex].Words = []Word{}
			}
			atomic.AddInt32(&completedSegments, int32(c.end-c.start))
			if onProgress != nil {
				onProgress(len(segments), int(atomic.LoadInt32(&completedSegments)), c.end, "done")
			}
		}(c)
	}

	wg.Wait()

	var translatedTexts []string
	for _, seg := range translatedSegments {
		if seg.Text != "" {
			translatedTexts = append(translatedTexts, seg.Text)
		}
	}

	if firstErr != nil {
		okSegs := len(segments) - int(atomic.LoadInt32(&failedSegments))
		detailLines := strings.Join(errs, "\n")
		summary := fmt.Sprintf("%d/%d segments failed (%d/%d chunks). %d segments OK.\nDetails:\n%s",
			atomic.LoadInt32(&failedSegments), len(segments), len(errs), totalChunks, okSegs, detailLines)
		return Translation{
			SourceLanguage: source,
			TargetLanguage: target,
			Status:         TranscriptionStatusTranslationError,
			Engine:         "llm",
			Result: WhisperResult{
				Language: target,
				Duration: duration,
				Segments: translatedSegments,
				Text:     strings.Join(translatedTexts, "\n"),
			},
		}, fmt.Errorf("%s", summary)
	}

	return Translation{
		SourceLanguage: source,
		TargetLanguage: target,
		Status:         TranscriptionStatusDone,
		Engine:         "llm",
		Result: WhisperResult{
			Language: target,
			Duration: duration,
			Segments: translatedSegments,
			Text:     strings.Join(translatedTexts, "\n"),
		},
	}, nil
}

func translateChunkedWithSessionHistory(ctx context.Context, translator *llmTranslator, segments []Segment, chunks []llmChunkDef, source, target, extraContext string, translatedSegments []Segment, historyChunks, totalChunks int, duration float64, onProgress TranslationProgressCallback) (Translation, error) {
	var firstErr error
	var errs []string
	var translatedTexts []string
	var history []llmChunkHistory
	completedSegments := 0
	failedSegments := 0

	for _, c := range chunks {
		select {
		case <-ctx.Done():
			return buildChunkedTranslation(source, target, duration, translatedSegments, TranscriptionStatusTranslationError), ctx.Err()
		default:
		}

		chunkSegs := segments[c.start:c.end]
		log.Info().Int("chunk", c.index+1).Int("historyChunks", len(history)).Int("segmentStart", c.start+1).Int("segmentEnd", c.end).Msg("LLM translating chunk with session history")
		items, chunkErr := translator.translateChunkWithHistory(ctx, chunkSegs, source, target, extraContext, history)
		if chunkErr != nil {
			log.Warn().Err(chunkErr).Int("chunk", c.index+1).Str("endpoint", translator.endpoint).Str("model", translator.model).Str("segments", fmt.Sprintf("%d-%d", c.start+1, c.end)).Msg("LLM chunk translation failed")
			if firstErr == nil {
				firstErr = chunkErr
			}
			errs = append(errs, fmt.Sprintf("chunk %d (segments %d-%d): %s", c.index+1, c.start+1, c.end, chunkErr))
			failed := c.end - c.start
			failedSegments += failed
			completedSegments += failed
			if onProgress != nil {
				onProgress(len(segments), completedSegments, c.start+1, "error")
			}
			if failedSegments >= 60 {
				break
			}
			continue
		}

		for i, item := range items {
			if i >= c.end-c.start {
				break
			}
			segmentIndex := c.start + i
			translatedSegments[segmentIndex].Text = strings.TrimSpace(item.Text)
			translatedSegments[segmentIndex].Words = []Word{}
		}
		history = append(history, llmChunkHistory{segments: chunkSegs, translations: items[:minInt(len(items), len(chunkSegs))]})
		if len(history) > historyChunks {
			history = history[len(history)-historyChunks:]
		}
		completedSegments += c.end - c.start
		if onProgress != nil {
			onProgress(len(segments), completedSegments, c.end, "done")
		}
	}

	for _, seg := range translatedSegments {
		if seg.Text != "" {
			translatedTexts = append(translatedTexts, seg.Text)
		}
	}

	if firstErr != nil {
		okSegs := len(segments) - failedSegments
		detailLines := strings.Join(errs, "\n")
		summary := fmt.Sprintf("%d/%d segments failed (%d/%d chunks). %d segments OK.\nDetails:\n%s", failedSegments, len(segments), len(errs), totalChunks, okSegs, detailLines)
		return Translation{
			SourceLanguage: source,
			TargetLanguage: target,
			Status:         TranscriptionStatusTranslationError,
			Engine:         "llm",
			Result: WhisperResult{
				Language: target,
				Duration: duration,
				Segments: translatedSegments,
				Text:     strings.Join(translatedTexts, "\n"),
			},
		}, fmt.Errorf("%s", summary)
	}

	return buildChunkedTranslation(source, target, duration, translatedSegments, TranscriptionStatusDone), nil
}

func buildChunkedTranslation(source, target string, duration float64, translatedSegments []Segment, status int) Translation {
	var translatedTexts []string
	for _, seg := range translatedSegments {
		if seg.Text != "" {
			translatedTexts = append(translatedTexts, seg.Text)
		}
	}
	return Translation{
		SourceLanguage: source,
		TargetLanguage: target,
		Status:         status,
		Engine:         "llm",
		Result: WhisperResult{
			Language: target,
			Duration: duration,
			Segments: translatedSegments,
			Text:     strings.Join(translatedTexts, "\n"),
		},
	}
}

func (t *llmTranslator) translateSegment(ctx context.Context, seg Segment, source, target, extraContext string) (string, error) {
	if text, ok := t.getCachedSegment(source, target, extraContext, seg.Text); ok {
		return text, nil
	}
	systemPrompt := buildLLMSystemPrompt(source, target, extraContext)
	userPrompt := fmt.Sprintf("Translate this subtitle:\n%s", seg.Text)

	if t.useTools {
		text, err := t.translateSegmentWithTool(ctx, systemPrompt, userPrompt, seg)
		if err == nil {
			t.setCachedSegment(source, target, extraContext, seg.Text, text)
			return text, nil
		}
		if t.useTools {
			return "", err
		}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}

		content, err := t.chat(ctx, systemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		text := strings.TrimSpace(content)
		if text == "" {
			lastErr = fmt.Errorf("empty translation")
			continue
		}
		t.setCachedSegment(source, target, extraContext, seg.Text, text)
		return text, nil
	}
	log.Warn().Err(lastErr).Int("segment", seg.Index()).Str("text", excerpt(seg.Text, 100)).Str("model", t.model).Str("targetLang", target).Msg("LLM segment exhausted all retries")
	return "", lastErr
}

func (t *llmTranslator) translateSegmentWithTool(ctx context.Context, systemPrompt, userPrompt string, seg Segment) (string, error) {
	body, err := t.marshalChatRequest([]message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, []toolDefinition{translateSegmentTool})
	if err != nil {
		return "", err
	}

	respBody, err := t.requestWithBackoff(ctx, body, true)
	if err != nil {
		return "", err
	}

	var result functionCallResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse tool response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	msg := result.Choices[0].Message

	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "translate_segment" {
			var args struct {
				Index          int    `json:"index"`
				TranslatedText string `json:"translated_text"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return "", fmt.Errorf("parse tool args: %w", err)
			}
			return args.TranslatedText, nil
		}
	}

	if msg.Content != "" {
		t.useTools = false
		return "", fmt.Errorf("model did not use tools, fallback to chat")
	}

	return "", fmt.Errorf("no translation in response")
}

func (t *llmTranslator) translateChunk(ctx context.Context, segments []Segment, source, target, extraContext string) ([]llmSegmentResponse, error) {
	results := make([]llmSegmentResponse, len(segments))
	requestSegments := make([]Segment, 0, len(segments))
	requestPositions := make([]int, 0, len(segments))
	for i, seg := range segments {
		if text, ok := t.getCachedSegment(source, target, extraContext, seg.Text); ok {
			results[i] = llmSegmentResponse{Text: text}
			continue
		}
		requestSegments = append(requestSegments, seg)
		requestPositions = append(requestPositions, i)
	}
	if len(requestSegments) == 0 {
		return results, nil
	}

	systemPrompt := buildLLMSystemPrompt(source, target, extraContext)
	userPrompt := buildChunkUserPrompt(requestSegments)

	if t.useTools {
		items, err := t.translateChunkWithTool(ctx, systemPrompt, userPrompt, requestSegments)
		if err == nil {
			missing := missingTranslationCount(items, requestSegments)
			if missing == 0 {
				t.cacheChunkResults(source, target, extraContext, requestSegments, items)
				return mergeChunkResults(results, requestPositions, items), nil
			}
			log.Debug().Int("missing", missing).Int("expected", len(requestSegments)).Msg("LLM tool call returned incomplete translations, falling back to chat parsing")
			t.useTools = false
			err = fmt.Errorf("incomplete tool translations: %d missing", missing)
		}
		// Tools failed or model didn't use them — try parsing the response as text
		if t.useTools {
			return nil, err
		}
	}

	// Try chat + || format parsing first (fast, one API call)
	content, err := t.chat(ctx, systemPrompt, userPrompt)
	if err == nil {
		items, parseErr := t.parseChunkResult(content, requestSegments)
		if parseErr == nil && len(items) >= len(requestSegments) {
			items = items[:len(requestSegments)]
			t.cacheChunkResults(source, target, extraContext, requestSegments, items)
			return mergeChunkResults(results, requestPositions, items), nil
		}
	}

	// Last resort: translate each segment individually with retry
	for i, seg := range requestSegments {
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(attempt) * time.Second):
				}
			}
			text, err := t.translateSegment(ctx, seg, source, target, extraContext)
			if err == nil && strings.TrimSpace(text) != "" {
				results[requestPositions[i]] = llmSegmentResponse{Text: text}
				lastErr = nil
				break
			}
			lastErr = err
		}
		if lastErr != nil {
			log.Warn().Err(lastErr).Int("segment", i+1).Str("text", excerpt(seg.Text, 100)).Str("model", t.model).Msg("LLM per-segment fallback failed in chunk")
			return nil, fmt.Errorf("segment %d: %w", i+1, lastErr)
		}
	}
	return results, nil
}

func (t *llmTranslator) translateChunkWithHistory(ctx context.Context, segments []Segment, source, target, extraContext string, history []llmChunkHistory) ([]llmSegmentResponse, error) {
	systemPrompt := buildLLMSystemPrompt(source, target, extraContext)
	messages := []message{{Role: "system", Content: systemPrompt}}
	for _, h := range history {
		if len(h.segments) == 0 || len(h.translations) == 0 {
			continue
		}
		messages = append(messages,
			message{Role: "user", Content: buildStrictChunkUserPrompt(h.segments)},
			message{Role: "assistant", Content: buildChunkTranslationResponse(h.segments, h.translations)},
		)
	}
	messages = append(messages, message{Role: "user", Content: buildStrictChunkUserPrompt(segments)})

	content, err := t.chatWithMessages(ctx, messages)
	if err == nil {
		items, parseErr := t.parseChunkResult(content, segments)
		if parseErr == nil && len(items) >= len(segments) {
			return items[:len(segments)], nil
		}
	}

	return t.translateChunk(ctx, segments, source, target, extraContext)
}

func (t *llmTranslator) translateChunkWithTool(ctx context.Context, systemPrompt, userPrompt string, expectedSegments []Segment) ([]llmSegmentResponse, error) {
	expectedCount := len(expectedSegments)
	body, err := t.marshalChatRequest([]message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, []toolDefinition{translateBatchTool})
	if err != nil {
		return nil, err
	}

	respBody, err := t.requestWithBackoff(ctx, body, true)
	if err != nil {
		return nil, err
	}

	var result functionCallResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse batch tool response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	msg := result.Choices[0].Message

	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "translate_batch" {
			var args struct {
				Translations []struct {
					Index          int    `json:"index"`
					TranslatedText string `json:"translated_text"`
				} `json:"translations"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("parse batch tool args: %w", err)
			}
			result := make([]llmSegmentResponse, expectedCount)
			startIdx := expectedSegmentStartNumber(expectedSegments)
			for _, tr := range args.Translations {
				slot := tr.Index - startIdx
				if slot < 0 || slot >= expectedCount {
					if tr.Index >= 1 && tr.Index <= expectedCount && startIdx != 1 {
						slot = tr.Index - 1
					} else {
						continue
					}
				}
				if strings.TrimSpace(tr.TranslatedText) != "" && result[slot].Text == "" {
					result[slot] = llmSegmentResponse{Text: tr.TranslatedText}
				}
			}
			return result, nil
		}
	}

	if msg.Content != "" {
		t.useTools = false
		return nil, fmt.Errorf("model did not use tools, fallback to chat")
	}

	return nil, fmt.Errorf("no translations in response")
}

func (t *llmTranslator) chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return t.chatWithMessages(ctx, []message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	})
}

func (t *llmTranslator) chatWithMessages(ctx context.Context, messages []message) (string, error) {
	body, err := t.marshalChatRequest(messages, nil)
	if err != nil {
		return "", err
	}

	respBody, err := t.requestWithBackoff(ctx, body, false)
	if err != nil {
		return "", err
	}

	var result functionCallResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func (t *llmTranslator) marshalChatRequest(messages []message, tools []toolDefinition) ([]byte, error) {
	body := map[string]interface{}{
		"model":       t.model,
		"messages":    messages,
		"stream":      false,
		"temperature": 0.2,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	for key, value := range t.thinkingParams() {
		body[key] = value
	}
	return json.Marshal(body)
}

func (t *llmTranslator) thinkingParams() map[string]interface{} {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_TRANSLATION_THINKING")))
	if mode == "auto" || mode == "omit" {
		return nil
	}
	enabled := mode == "on" || mode == "true" || mode == "enabled" || mode == "1"
	effort := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_TRANSLATION_THINKING_EFFORT")))
	if effort != "low" && effort != "medium" && effort != "high" {
		effort = "high"
	}

	model := strings.ToLower(t.model)
	endpoint := strings.ToLower(t.endpoint)

	// DeepSeek / Kimi / Seed / GLM style binary switch. DeepSeek also accepts
	// reasoning_effort on enabled requests, but disabling with only thinking.type
	// is safest for broad OpenAI-compatible gateways.
	if strings.Contains(model, "deepseek") || strings.Contains(model, "kimi") || strings.Contains(model, "moonshot") || strings.Contains(model, "glm") || strings.Contains(model, "zhipu") || strings.Contains(model, "doubao") || strings.Contains(model, "seed") || strings.Contains(endpoint, "deepseek") {
		if enabled {
			return map[string]interface{}{"thinking": map[string]string{"type": "enabled"}, "reasoning_effort": effort}
		}
		return map[string]interface{}{"thinking": map[string]string{"type": "disabled"}}
	}

	// OpenAI GPT-5 family and xAI Grok use reasoning_effort.
	if strings.Contains(model, "gpt-5") || strings.Contains(model, "gpt-chat") || strings.Contains(model, "grok") {
		if enabled {
			if strings.Contains(model, "grok") && effort == "medium" {
				effort = "low"
			}
			return map[string]interface{}{"reasoning_effort": effort}
		}
		return map[string]interface{}{"reasoning_effort": "none"}
	}

	// Qwen3-style switch. Budget is intentionally modest for subtitle work.
	if strings.Contains(model, "qwen") {
		if enabled {
			budget := map[string]int{"low": 1024, "medium": 4096, "high": 8192}[effort]
			return map[string]interface{}{"enable_thinking": true, "thinking_budget": budget}
		}
		return map[string]interface{}{"enable_thinking": false}
	}

	// Unknown provider/model: omit by default. Sending a vendor-specific disable
	// field to an incompatible OpenAI-compatible server commonly causes 400/422.
	return nil
}

func (g *llmRateLimitGate) wait(ctx context.Context) error {
	g.mu.Lock()
	remaining := time.Until(g.until)
	g.mu.Unlock()
	if remaining <= 0 {
		return nil
	}
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(remaining + jitter):
		return nil
	}
}

func (g *llmRateLimitGate) trip(retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Before(g.until) {
		return
	}
	if retryAfter > 0 {
		g.cooldown = minDuration(retryAfter, 2*time.Minute)
	} else if !g.lastTripAt.IsZero() && now.Sub(g.lastTripAt) < 30*time.Second {
		g.cooldown = minDuration(g.cooldown*2, time.Minute)
	} else {
		g.cooldown = time.Second
	}
	g.lastTripAt = now
	g.until = now.Add(g.cooldown)
	log.Warn().Dur("cooldown", g.cooldown).Msg("LLM rate limit cooldown started")
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		return time.Until(when)
	}
	return 0
}

func isRetryableLLMStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status >= 500
}

func (c *llmMemoryTranslationCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.max <= 0 {
		return "", false
	}
	value, ok := c.items[key]
	return value, ok
}

func (c *llmMemoryTranslationCache) set(key string, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.max <= 0 {
		return
	}
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
	}
	c.items[key] = value
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

func (t *llmTranslator) segmentCacheKey(source, target, extraContext, text string) string {
	payload := strings.Join([]string{t.model, source, target, extraContext, text}, "\x00")
	sum := md5.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func (t *llmTranslator) cacheEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("LLM_TRANSLATION_CACHE"))) != "false"
}

func (t *llmTranslator) getCachedSegment(source, target, extraContext, text string) (string, bool) {
	if !t.cacheEnabled() || strings.TrimSpace(text) == "" {
		return "", false
	}
	return sharedLLMTranslationCache.get(t.segmentCacheKey(source, target, extraContext, text))
}

func (t *llmTranslator) setCachedSegment(source, target, extraContext, text, translation string) {
	if !t.cacheEnabled() || strings.TrimSpace(text) == "" || strings.TrimSpace(translation) == "" {
		return
	}
	sharedLLMTranslationCache.set(t.segmentCacheKey(source, target, extraContext, text), strings.TrimSpace(translation))
}

func (t *llmTranslator) cacheChunkResults(source, target, extraContext string, segments []Segment, items []llmSegmentResponse) {
	for i := range segments {
		if i >= len(items) {
			break
		}
		t.setCachedSegment(source, target, extraContext, segments[i].Text, items[i].Text)
	}
}

func mergeChunkResults(base []llmSegmentResponse, positions []int, items []llmSegmentResponse) []llmSegmentResponse {
	for i, pos := range positions {
		if i >= len(items) || pos < 0 || pos >= len(base) {
			continue
		}
		base[pos] = items[i]
	}
	return base
}

func (t *llmTranslator) requestWithBackoff(ctx context.Context, body []byte, allowToolsFallback bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			log.Info().Err(lastErr).Int("attempt", attempt+1).Int("maxRetries", t.maxRetries).Dur("backoff", backoff).Msg("LLM request retry")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		if t.debugIO {
			log.Debug().Int("attempt", attempt+1).Bool("tools", allowToolsFallback).Str("endpoint", t.endpoint).Str("model", t.model).Str("request", string(body)).Msg("LLM request")
		}

		if err := sharedLLMRateLimitGate.wait(ctx); err != nil {
			return nil, err
		}

		start := time.Now()
		resp, err := t.doRequest(ctx, body)
		if err != nil {
			lastErr = err
			log.Debug().Err(err).Int("attempt", attempt+1).Dur("duration", time.Since(start)).Msg("LLM request failed")
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			log.Debug().Err(readErr).Int("attempt", attempt+1).Int("status", resp.StatusCode).Dur("duration", time.Since(start)).Msg("LLM response read failed")
			continue
		}

		if t.debugIO {
			log.Debug().Int("attempt", attempt+1).Int("status", resp.StatusCode).Dur("duration", time.Since(start)).Str("response", string(respBody)).Msg("LLM response")
		}

		if resp.StatusCode >= 400 {
			if allowToolsFallback && resp.StatusCode == 400 {
				t.useTools = false
				t.toolsTried = true
				t.resetHTTPClient()
				log.Debug().Int("status", resp.StatusCode).Str("response", excerpt(string(respBody), 500)).Msg("LLM function calling not supported, falling back to chat completion")
				return nil, fmt.Errorf("tools not supported")
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, fmt.Errorf("auth error %d: %s", resp.StatusCode, excerpt(string(respBody), 500))
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				sharedLLMRateLimitGate.trip(parseRetryAfter(resp.Header.Get("Retry-After")))
			}
			if isRetryableLLMStatus(resp.StatusCode) {
				lastErr = fmt.Errorf("api error %d: %s", resp.StatusCode, excerpt(string(respBody), 200))
				log.Debug().Err(lastErr).Int("attempt", attempt+1).Int("status", resp.StatusCode).Str("response", excerpt(string(respBody), 500)).Dur("duration", time.Since(start)).Msg("LLM retryable API error")
				continue
			}
			return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, excerpt(string(respBody), 500))
		}

		return normalizeResponse(respBody), nil
	}
	if lastErr != nil {
		log.Warn().Err(lastErr).Str("endpoint", t.endpoint).Str("model", t.model).Bool("tools", allowToolsFallback).Int("attempts", t.maxRetries+1).Str("request", excerpt(string(body), 300)).Msg("LLM request exhausted all retries")
	}
	return nil, lastErr
}

func (t *llmTranslator) resetHTTPClient() {
	t.httpClient.CloseIdleConnections()
}

func (t *llmTranslator) doRequest(ctx context.Context, body []byte) (*http.Response, error) {
	httpCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	return t.httpClient.Do(req)
}

func (t *llmTranslator) parseChunkResult(content string, expectedSegments []Segment) ([]llmSegmentResponse, error) {
	expectedCount := len(expectedSegments)
	if expectedCount == 0 {
		return nil, nil
	}
	startIdx := expectedSegmentStartNumber(expectedSegments)
	byIndex := make([]string, expectedCount)
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, "||")
		if idx >= 0 {
			num, err := strconv.Atoi(strings.TrimSpace(line[:idx]))
			if err != nil {
				continue
			}
			text := strings.TrimSpace(line[idx+2:])
			slot := num - startIdx
			if slot < 0 || slot >= expectedCount {
				// Common LLM failure: renumber 1..N even when real segment ids are
				// larger. Accept it only when the whole chunk starts at 1.
				if num >= 1 && num <= expectedCount && startIdx != 1 {
					slot = num - 1
				} else {
					continue
				}
			}
			if text != "" && byIndex[slot] == "" {
				byIndex[slot] = text
			}
		}
	}
	applyChunkMergeGuard(byIndex, expectedSegments)
	if completeCount(byIndex, expectedSegments) == expectedCount {
		return segmentResponsesFromStrings(byIndex), nil
	}

	// If || format didn't yield enough results, accept plain lines only when the
	// model returned exactly one non-explanatory line per segment. Positional
	// guessing on a different line count silently shifts subtitle timestamps.
	plain := make([]string, 0, expectedCount)
	if completeCount(byIndex, expectedSegments) == 0 {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "```") {
				continue
			}
			// Skip lines that look like markdown or explanations
			if strings.Contains(line, "||") {
				continue
			}
			plain = append(plain, line)
		}
		if len(plain) == expectedCount {
			return segmentResponsesFromStrings(plain), nil
		}
	}

	// Retry with explicit format
	for retry := 0; retry < 1; retry++ {
		explicit := make([]string, expectedCount)
		for i, seg := range expectedSegments {
			explicit[i] = fmt.Sprintf("%d||%s", startIdx+i, strings.ReplaceAll(seg.Text, "\n", " "))
		}
		systemPrompt := buildLLMSystemPrompt("", "", "")
		retryPrompt := fmt.Sprintf("Translate these subtitle lines one by one. Respond with EXACTLY %d lines, each in format N||translated text.\n%s", expectedCount, strings.Join(explicit, "\n"))
		retryContent, err := t.chat(context.Background(), systemPrompt, retryPrompt)
		if err != nil {
			continue
		}
		retrySlots := make([]string, expectedCount)
		for _, line := range strings.Split(retryContent, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			idx := strings.Index(line, "||")
			if idx < 0 {
				continue
			}
			num, err := strconv.Atoi(strings.TrimSpace(line[:idx]))
			if err != nil {
				continue
			}
			text := strings.TrimSpace(line[idx+2:])
			if text == "" {
				continue
			}
			slot := num - startIdx
			if slot >= 0 && slot < expectedCount && retrySlots[slot] == "" {
				retrySlots[slot] = text
			}
		}
		applyChunkMergeGuard(retrySlots, expectedSegments)
		if completeCount(retrySlots, expectedSegments) == expectedCount {
			return segmentResponsesFromStrings(retrySlots), nil
		}
	}

	return nil, fmt.Errorf("llm returned %d complete lines, expected %d", completeCount(byIndex, expectedSegments), expectedCount)
}

func expectedSegmentStartNumber(segments []Segment) int {
	if len(segments) > 0 && segments[0].ID != "" {
		if n, err := strconv.Atoi(segments[0].ID); err == nil {
			return n + 1
		}
	}
	return 1
}

func applyChunkMergeGuard(lines []string, expectedSegments []Segment) {
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" || strings.TrimSpace(expectedSegments[i].Text) == "" {
			continue
		}
		j := i - 1
		for j >= 0 && strings.TrimSpace(expectedSegments[j].Text) == "" {
			j--
		}
		if j >= 0 && strings.TrimSpace(lines[j]) != "" {
			lines[j] = ""
		}
	}
}

func completeCount(lines []string, expectedSegments []Segment) int {
	count := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != "" || (i < len(expectedSegments) && strings.TrimSpace(expectedSegments[i].Text) == "") {
			count++
		}
	}
	return count
}

func segmentResponsesFromStrings(lines []string) []llmSegmentResponse {
	items := make([]llmSegmentResponse, len(lines))
	for i, line := range lines {
		items[i] = llmSegmentResponse{Text: strings.TrimSpace(line)}
	}
	return items
}

func missingTranslationCount(items []llmSegmentResponse, expectedSegments []Segment) int {
	missing := 0
	for i := range expectedSegments {
		if strings.TrimSpace(expectedSegments[i].Text) == "" {
			continue
		}
		if i >= len(items) || strings.TrimSpace(items[i].Text) == "" {
			missing++
		}
	}
	return missing
}

func buildChunkUserPrompt(segments []Segment) string {
	return "Translate each subtitle line below.\n" +
		"Return EXACTLY one output line for each input line.\n" +
		"Keep the same number prefix and format: N||translated text\n" +
		"Do not merge adjacent lines, even when they form one sentence.\n" +
		"Format example only:\n" +
		"1||<translation of input line 1>\n" +
		"2||<translation of input line 2>\n\n" +
		"Actual input:\n" +
		buildNumberedSegments(segments)
}

func buildStrictChunkUserPrompt(segments []Segment) string {
	return "Translate each subtitle line below.\n" +
		"Return EXACTLY one line per input line.\n" +
		"Return ONLY this format: N||translated text\n" +
		"Do not add markdown, bullets, notes, original text, or blank lines.\n" +
		"Keep the same numbering and order.\n" +
		"Never merge multiple numbered source lines into one translation line.\n" +
		"A translated line may be empty only if the matching source line is empty.\n" +
		buildNumberedSegments(segments)
}

func buildNumberedSegments(segments []Segment) string {
	startIdx := 1
	if len(segments) > 0 && segments[0].ID != "" {
		if n, err := strconv.Atoi(segments[0].ID); err == nil {
			startIdx = n + 1
		}
	}

	var sb strings.Builder
	for i, seg := range segments {
		idx := startIdx + i
		sb.WriteString(fmt.Sprintf("%d||%s\n", idx, seg.Text))
	}
	return sb.String()
}

func buildChunkTranslationResponse(segments []Segment, translations []llmSegmentResponse) string {
	startIdx := 1
	if len(segments) > 0 && segments[0].ID != "" {
		if n, err := strconv.Atoi(segments[0].ID); err == nil {
			startIdx = n + 1
		}
	}

	var sb strings.Builder
	for i, tr := range translations {
		sb.WriteString(fmt.Sprintf("%d||%s\n", startIdx+i, strings.TrimSpace(tr.Text)))
	}
	return sb.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func estimateSubtitleTokens(text string) int {
	runes := []rune(text)
	if len(runes) == 0 {
		return 0
	}
	cjk := 0
	for _, r := range runes {
		if (r >= 0x3400 && r <= 0x4dbf) || (r >= 0x4e00 && r <= 0x9fff) || (r >= 0xf900 && r <= 0xfaff) {
			cjk++
		}
	}
	other := len(runes) - cjk
	return int(math.Ceil(float64(cjk)*0.5 + float64(other)*0.25))
}

func (t *llmTranslator) modelContextLimit() int {
	model := strings.ToLower(t.model)
	endpoint := strings.ToLower(t.endpoint)
	switch {
	case strings.Contains(model, "gemini"):
		return 1000000
	case strings.Contains(model, "claude"):
		return 200000
	case strings.Contains(model, "deepseek"), strings.Contains(endpoint, "deepseek"):
		return 128000
	case strings.Contains(model, "qwen"), strings.Contains(model, "kimi"), strings.Contains(model, "moonshot"), strings.Contains(model, "gpt-5"), strings.Contains(model, "grok"), strings.Contains(model, "mistral"):
		return 128000
	case strings.Contains(model, "minimax"):
		return 262000
	default:
		if v := envInt("LLM_TRANSLATION_CONTEXT_LIMIT", 0); v > 0 {
			return v
		}
		return defaultLLMContextLimit
	}
}

func (t *llmTranslator) adaptiveChunkSize(segments []Segment, requested int) int {
	if requested <= 0 || len(segments) == 0 {
		return requested
	}
	totalTokens := 0
	for _, seg := range segments {
		totalTokens += estimateSubtitleTokens(seg.Text) + 6 // line number + delimiters
	}
	avgTokens := maxInt(1, totalTokens/len(segments))
	limit := t.modelContextLimit()
	reserved := 900 + estimateSubtitleTokens(buildLLMSystemPrompt("", "", ""))
	safeTokens := int(float64(limit)*llmPromptSafetyRatio) - reserved
	if safeTokens <= 0 {
		return requested
	}
	safeChunkSize := maxInt(5, safeTokens/avgTokens)
	if safeChunkSize < requested {
		log.Info().Int("requested", requested).Int("adaptive", safeChunkSize).Int("avgTokensPerSegment", avgTokens).Int("contextLimit", limit).Str("model", t.model).Msg("LLM chunk size reduced to fit context window")
		return safeChunkSize
	}
	return requested
}

func normalizeResponse(body []byte) []byte {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return body
	}

	if idx := strings.Index(s, "data: [DONE]"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return []byte(s)
}

func buildCombinedContext(userContext string, autoContext string, referenceExamples string) string {
	var parts []string
	if strings.TrimSpace(autoContext) != "" {
		parts = append(parts, strings.TrimSpace(autoContext))
	}
	if strings.TrimSpace(userContext) != "" {
		parts = append(parts, strings.TrimSpace(userContext))
	}
	if strings.TrimSpace(referenceExamples) != "" {
		parts = append(parts, strings.TrimSpace(referenceExamples))
	}
	return strings.Join(parts, "\n\n")
}

func buildLLMSystemPrompt(source string, target string, extraContext string) string {
	parts := []string{
		"You are a professional film and TV subtitle translator.",
		fmt.Sprintf("Translate from %s to %s.", source, target),
		"Output ONLY translations — no explanations, no original text, no markdown.",
		"Translate naturally for the target audience based on context.",
		"Preserve names, places, brands, and terminology consistently.",
		"Keep music markers (♪) and sound descriptions when present.",
		"Preserve subtitle segmentation: never merge, split, reorder, or omit numbered lines.",
		"Keep translations concise enough for subtitles while preserving meaning.",
	}
	if hint := buildTargetLanguageHint(target); hint != "" {
		parts = append(parts, hint)
	}
	if strings.TrimSpace(extraContext) != "" {
		parts = append(parts, "Context and terminology:", strings.TrimSpace(extraContext))
	}
	return strings.Join(parts, "\n")
}

func buildTargetLanguageHint(target string) string {
	lang := strings.ToLower(strings.TrimSpace(target))
	switch lang {
	case "zh", "zh-cn", "zh_cn", "chinese", "simplified chinese", "mandarin":
		return "For Chinese subtitles: use natural spoken Chinese, avoid overly formal written style, and keep lines short."
	case "zh-tw", "zh_tw", "traditional chinese":
		return "For Traditional Chinese subtitles: use natural spoken Traditional Chinese and keep lines short."
	case "ja", "japanese":
		return "For Japanese subtitles: use natural conversational Japanese and appropriate politeness based on character relationships."
	case "ko", "korean":
		return "For Korean subtitles: use natural conversational Korean with appropriate honorifics."
	case "en", "english":
		return "For English subtitles: use natural idiomatic English, not literal word-for-word phrasing."
	default:
		return ""
	}
}

func excerpt(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// Function calling types

type toolDefinition struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  toolParameters `json:"parameters"`
}

type toolParameters struct {
	Type       string                  `json:"type"`
	Properties map[string]toolProperty `json:"properties"`
	Required   []string                `json:"required"`
}

type toolProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Items       *itemDef `json:"items,omitempty"`
}

type itemDef struct {
	Type       string                  `json:"type"`
	Properties map[string]toolProperty `json:"properties,omitempty"`
}

var translateSegmentTool = toolDefinition{
	Type: "function",
	Function: toolFunction{
		Name:        "translate_segment",
		Description: "Translate a single subtitle segment from source to target language",
		Parameters: toolParameters{
			Type: "object",
			Properties: map[string]toolProperty{
				"index": {
					Type:        "integer",
					Description: "Segment index number",
				},
				"translated_text": {
					Type:        "string",
					Description: "The translated subtitle text in the target language",
				},
			},
			Required: []string{"index", "translated_text"},
		},
	},
}

var translateBatchTool = toolDefinition{
	Type: "function",
	Function: toolFunction{
		Name:        "translate_batch",
		Description: "Translate multiple subtitle segments in a batch. Use when there are multiple segments to translate together.",
		Parameters: toolParameters{
			Type: "object",
			Properties: map[string]toolProperty{
				"translations": {
					Type:        "array",
					Description: "Array of translated segments",
					Items: &itemDef{
						Type: "object",
						Properties: map[string]toolProperty{
							"index": {
								Type:        "integer",
								Description: "Segment index number",
							},
							"translated_text": {
								Type:        "string",
								Description: "The translated subtitle text in the target language",
							},
						},
					},
				},
			},
			Required: []string{"translations"},
		},
	},
}

type functionCallRequest struct {
	Model       string           `json:"model"`
	Messages    []message        `json:"messages"`
	Stream      bool             `json:"stream"`
	Tools       []toolDefinition `json:"tools,omitempty"`
	ToolChoice  interface{}      `json:"tool_choice,omitempty"`
	Temperature float64          `json:"temperature"`
	Thinking    interface{}      `json:"thinking,omitempty"`
}

var disableThinking = map[string]string{"type": "disabled"}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

func (s Segment) Index() int {
	if s.ID == "" {
		return 0
	}
	n, err := strconv.Atoi(s.ID)
	if err != nil {
		return 0
	}
	return n + 1
}
