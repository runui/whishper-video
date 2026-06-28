package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	type chunkDef struct {
		index int
		start int
		end   int
	}
	chunks := make([]chunkDef, totalChunks)
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(segments) {
			end = len(segments)
		}
		chunks[i] = chunkDef{index: i, start: start, end: end}
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
		go func(c chunkDef) {
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

func (t *llmTranslator) translateSegment(ctx context.Context, seg Segment, source, target, extraContext string) (string, error) {
	systemPrompt := buildLLMSystemPrompt(source, target, extraContext)
	userPrompt := fmt.Sprintf("Translate this subtitle:\n%s", seg.Text)

	if t.useTools {
		text, err := t.translateSegmentWithTool(ctx, systemPrompt, userPrompt, seg)
		if err == nil {
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
		return text, nil
	}
	log.Warn().Err(lastErr).Int("segment", seg.Index()).Str("text", excerpt(seg.Text, 100)).Str("model", t.model).Str("targetLang", target).Msg("LLM segment exhausted all retries")
	return "", lastErr
}

func (t *llmTranslator) translateSegmentWithTool(ctx context.Context, systemPrompt, userPrompt string, seg Segment) (string, error) {
	body, err := json.Marshal(functionCallRequest{
		Model: t.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:      false,
		Tools:       []toolDefinition{translateSegmentTool},
		Temperature: 0.2,
		Thinking:    disableThinking,
	})
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
	systemPrompt := buildLLMSystemPrompt(source, target, extraContext)
	userPrompt := buildChunkUserPrompt(segments)

	if t.useTools {
		items, err := t.translateChunkWithTool(ctx, systemPrompt, userPrompt, len(segments))
		if err == nil {
			return items, nil
		}
		// Tools failed or model didn't use them — try parsing the response as text
		if t.useTools {
			return nil, err
		}
	}

	// Try chat + || format parsing first (fast, one API call)
	content, err := t.chat(ctx, systemPrompt, userPrompt)
	if err == nil {
		items, parseErr := t.parseChunkResult(content, segments)
		if parseErr == nil && len(items) >= len(segments) {
			return items[:len(segments)], nil
		}
	}

	// Last resort: translate each segment individually with retry
	results := make([]llmSegmentResponse, len(segments))
	for i, seg := range segments {
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
				results[i] = llmSegmentResponse{Text: text}
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

func (t *llmTranslator) translateChunkWithTool(ctx context.Context, systemPrompt, userPrompt string, expectedCount int) ([]llmSegmentResponse, error) {
	body, err := json.Marshal(functionCallRequest{
		Model: t.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:      false,
		Tools:       []toolDefinition{translateBatchTool},
		Temperature: 0.2,
		Thinking:    disableThinking,
	})
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
			for _, tr := range args.Translations {
				if tr.Index >= 1 && tr.Index <= expectedCount {
					result[tr.Index-1] = llmSegmentResponse{Text: tr.TranslatedText}
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
	body, err := json.Marshal(functionCallRequest{
		Model: t.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:      false,
		Temperature: 0.2,
		Thinking:    disableThinking,
	})
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
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
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
	result := make([]llmSegmentResponse, 0, expectedCount)
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, "||")
		if idx >= 0 {
			text := strings.TrimSpace(line[idx+2:])
			if text != "" {
				result = append(result, llmSegmentResponse{Text: text})
			}
		}
	}

	// If || format didn't yield enough results, try plain lines
	if len(result) < expectedCount {
		result = result[:0]
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "```") {
				continue
			}
			// Skip lines that look like markdown or explanations
			if strings.Contains(line, "||") {
				continue
			}
			result = append(result, llmSegmentResponse{Text: line})
		}
	}

	if len(result) >= expectedCount {
		return result[:expectedCount], nil
	}

	// Retry with explicit format
	startIdx := 1
	if len(expectedSegments) > 0 && expectedSegments[0].ID != "" {
		if n, err := strconv.Atoi(expectedSegments[0].ID); err == nil {
			startIdx = n + 1
		}
	}
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
		retryResult := make([]llmSegmentResponse, 0, expectedCount)
		for _, line := range strings.Split(retryContent, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			idx := strings.Index(line, "||")
			if idx < 0 {
				continue
			}
			text := strings.TrimSpace(line[idx+2:])
			if text == "" {
				continue
			}
			retryResult = append(retryResult, llmSegmentResponse{Text: text})
		}
		if len(retryResult) >= expectedCount {
			return retryResult[:expectedCount], nil
		}
	}

	return nil, fmt.Errorf("llm returned %d lines, expected %d", len(result), expectedCount)
}

func buildChunkUserPrompt(segments []Segment) string {
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
	userText := sb.String()

	return fmt.Sprintf("Translate each line below. Keep the same line format with the number prefix.\n%s", userText)
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
	}
	if strings.TrimSpace(extraContext) != "" {
		parts = append(parts, "Context and terminology:", strings.TrimSpace(extraContext))
	}
	return strings.Join(parts, "\n")
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
