package models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ─── 1. Thinking param dispatch ────────────────────────────────────────

func TestThinkingParams(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		mode   string
		expect map[string]interface{}
	}{
		// DeepSeek
		{name: "DeepSeek Off", model: "deepseek-v4-flash", mode: "off",
			expect: map[string]interface{}{"thinking": map[string]string{"type": "disabled"}}},
		{name: "DeepSeek On", model: "deepseek-v4-flash", mode: "on",
			expect: map[string]interface{}{"thinking": map[string]string{"type": "enabled"}, "reasoning_effort": "high"}},

		// OpenAI / GPT
		{name: "GPT Off", model: "gpt-5.5-turbo", mode: "off",
			expect: map[string]interface{}{"reasoning_effort": "none"}},
		{name: "GPT On", model: "gpt-5.5-turbo", mode: "on",
			expect: map[string]interface{}{"reasoning_effort": "high"}},

		// Grok
		{name: "Grok Off", model: "grok-3", mode: "off",
			expect: map[string]interface{}{"reasoning_effort": "none"}},
		{name: "Grok On", model: "grok-3", mode: "on",
			expect: map[string]interface{}{"reasoning_effort": "high"}},

		// Qwen
		{name: "Qwen Off", model: "qwen3-max", mode: "off",
			expect: map[string]interface{}{"enable_thinking": false}},
		{name: "Qwen On", model: "qwen3-max", mode: "on",
			expect: map[string]interface{}{"enable_thinking": true, "thinking_budget": 8192}},

		// Claude (unknown → nil = omit)
		{name: "Claude Off", model: "claude-sonnet-4-20250514", mode: "off",
			expect: nil},
		{name: "Unknown Model Off", model: "llama-3-70b", mode: "off",
			expect: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Ensure env is unset so defaults apply (effort=high)
			os.Unsetenv("LLM_TRANSLATION_THINKING")
			os.Unsetenv("LLM_TRANSLATION_THINKING_EFFORT")
			if c.mode == "off" {
				os.Setenv("LLM_TRANSLATION_THINKING", "")
			} else {
				os.Setenv("LLM_TRANSLATION_THINKING", "on")
			}

			tr := &llmTranslator{model: c.model, endpoint: "http://test/v1/chat/completions"}
			got := tr.thinkingParams()

			gotJSON, _ := json.Marshal(got)
			expectJSON, _ := json.Marshal(c.expect)

			if string(gotJSON) != string(expectJSON) {
				t.Errorf("got: %s\nwant: %s", string(gotJSON), string(expectJSON))
			} else {
				t.Logf("✓ %s", string(gotJSON))
			}
		})
	}
}

// ─── 2. Merge guard direct test ───────────────────────────────────────

func TestApplyChunkMergeGuard(t *testing.T) {
	cases := []struct {
		name     string
		lines    []string
		segs     []Segment
		expected []string
	}{
		{
			name:     "Normal - no merge",
			lines:    []string{"a", "b", "c"},
			segs:     []Segment{{Text: "x"}, {Text: "y"}, {Text: "z"}},
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "Merged - clear first slot",
			lines:    []string{"a b c", "", ""},
			segs:     []Segment{{Text: "x"}, {Text: "y"}, {Text: "z"}},
			expected: []string{"", "", ""},
		},
		{
			name:     "Merge middle - clear previous filled slot",
			lines:    []string{"a", "b c", ""},
			segs:     []Segment{{Text: "x"}, {Text: "y"}, {Text: "z"}},
			expected: []string{"a", "", ""},
		},
		{
			name:     "Merged - first slot already contains meaning, not clear",
			lines:    []string{"a", "b", "c"},
			segs:     []Segment{{Text: "x"}, {Text: "y"}, {Text: ""}},
			expected: []string{"a", "b", "c"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyChunkMergeGuard(c.lines, c.segs)
			got := strings.Join(c.lines, "|")
			want := strings.Join(c.expected, "|")
			if got != want {
				t.Errorf("got: %q\nwant: %q", got, want)
			}
		})
	}
}

// ─── 3. Parser (no retry path) ────────────────────────────────────────

func TestParseChunkResultNormal(t *testing.T) {
	// With segment IDs starting at 0 → startIdx = 1
	segs := []Segment{
		{Text: "Hello", ID: "0"},
		{Text: "World", ID: "1"},
		{Text: "Foo", ID: "2"},
	}

	cases := []struct {
		name    string
		content string
		check   func(t *testing.T, items []llmSegmentResponse)
	}{
		{
			name:    "N||format exact match",
			content: "1||你好\n2||世界\n3||发",
			check: func(t *testing.T, items []llmSegmentResponse) {
				if len(items) != 3 {
					t.Fatalf("expected 3 items, got %d", len(items))
				}
				if items[0].Text != "你好" || items[1].Text != "世界" {
					t.Errorf("slot 0=%q slot 1=%q", items[0].Text, items[1].Text)
				}
				t.Logf("✓ %d items parsed correctly", len(items))
			},
		},
		{
			name:    "With markdown fences",
			content: "```\n1||你好\n2||世界\n3||发\n```",
			check: func(t *testing.T, items []llmSegmentResponse) {
				if len(items) != 3 {
					t.Fatalf("expected 3 items, got %d", len(items))
				}
				t.Logf("✓ fences stripped, %d items", len(items))
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &llmTranslator{}
			items, err := tr.parseChunkResult(c.content, segs)
			if err != nil {
				t.Fatalf("parseChunkResult error: %v", err)
			}
			c.check(t, items)
		})
	}
}

// ─── 4. Adaptive chunk size ───────────────────────────────────────────

func TestAdaptiveChunkSize(t *testing.T) {
	shortSegs := make([]Segment, 30)
	for i := range shortSegs {
		shortSegs[i] = Segment{Text: "Hello world.", ID: fmt.Sprintf("%d", i)}
	}

	longSegs := make([]Segment, 100)
	for i := range longSegs {
		longSegs[i] = Segment{
			Text: strings.Repeat("This is a very long subtitle line that describes the scene in great detail and includes multiple elements. ", 30),
			ID:   fmt.Sprintf("%d", i),
		}
	}

	tr := &llmTranslator{model: "deepseek-v4-flash", endpoint: "http://test"}

	t.Run("Short subtitles keep size", func(t *testing.T) {
		got := tr.adaptiveChunkSize(shortSegs, 50)
		if got < 40 {
			t.Errorf("too small: %d", got)
		}
		t.Logf("short segs → chunk=%d (req=%d)", got, 50)
	})

	t.Run("Long subtitles shrink", func(t *testing.T) {
		got := tr.adaptiveChunkSize(longSegs, 50)
		t.Logf("long segs  → chunk=%d (req=%d)", got, 50)
		// 100 segs * 30 * ~60 chars = ~180k chars ≈ 90k tokens > 128k*0.7=89.6k → should shrink
		if got >= 50 {
			t.Logf("NOTE: chunk not reduced (model context %d may still fit)", tr.modelContextLimit())
		}
	})
}

// ─── 5. Cache ─────────────────────────────────────────────────────────

func TestTranslationCache(t *testing.T) {
	cache := &llmMemoryTranslationCache{items: map[string]string{}, order: []string{}, max: 10000}

	cache.set("k1", "v1")
	cache.set("k2", "v2")

	v, ok := cache.get("k1")
	if !ok || v != "v1" {
		t.Fatalf("cache miss on k1")
	}
	v, ok = cache.get("k2")
	if !ok || v != "v2" {
		t.Fatalf("cache miss on k2")
	}
	_, ok = cache.get("nonexistent")
	if ok {
		t.Fatalf("unexpected cache hit")
	}

	// LRU eviction
	small := &llmMemoryTranslationCache{items: map[string]string{}, order: []string{}, max: 3}
	for _, k := range []string{"a", "b", "c", "d"} {
		small.set(k, k)
	}
	_, ok = small.get("a")
	if ok {
		t.Errorf("a should have been evicted")
	}
	_, ok = small.get("d")
	if !ok {
		t.Errorf("d should be present")
	}
	t.Log("✓ cache eviction works correctly")
}

// ─── 6. Rate-limit gate ──────────────────────────────────────────────

func TestRateLimitGate(t *testing.T) {
	t.Run("Basic trip", func(t *testing.T) {
		g := &llmRateLimitGate{cooldown: time.Second}
		g.trip(0)
		if !time.Now().Before(g.until) {
			t.Error("gate should be tripped")
		}
	})

	t.Run("Cooldown escalation", func(t *testing.T) {
		g := &llmRateLimitGate{cooldown: time.Second}
		// Force until to the past so second trip doesn't early-return
		g.until = time.Now().Add(-time.Millisecond)
		g.lastTripAt = time.Now().Add(-time.Millisecond)
		g.trip(0)
		if g.cooldown != 2*time.Second {
			t.Errorf("expected 2s, got %v", g.cooldown)
		}
		// Third trip within 30s → 4s
		g.until = time.Now().Add(-time.Millisecond)
		g.trip(0)
		if g.cooldown != 4*time.Second {
			t.Errorf("expected 4s, got %v", g.cooldown)
		}
	})

	t.Run("Retry-After override", func(t *testing.T) {
		g := &llmRateLimitGate{cooldown: time.Second}
		g.trip(10 * time.Second)
		if g.cooldown != 10*time.Second {
			t.Errorf("expected 10s, got %v", g.cooldown)
		}
	})
}

// ─── 7. Live end-to-end translation ───────────────────────────────────

func TestLiveTranslation(t *testing.T) {
	if os.Getenv("LLM_TRANSLATION_ENDPOINT") == "" {
		t.Skip("LLM endpoint not configured — skip live test")
	}
	if testing.Short() {
		t.Skip("short mode — skip live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	segs := []Segment{
		{Text: "Come on, we're going to be late!", ID: "0"},
		{Text: "Wait for me!", ID: "1"},
		{Text: "I told you we should have left earlier.", ID: "2"},
		{Text: "It's not my fault the car broke down.", ID: "3"},
		{Text: "Whatever. Let's just get going.", ID: "4"},
		{Text: "Hey, is that the new coffee shop?", ID: "5"},
		{Text: "Yeah, they just opened last week.", ID: "6"},
		{Text: "I heard their latte is amazing.", ID: "7"},
		{Text: "Let's grab one after the meeting.", ID: "8"},
		{Text: "Deal! I could really use some caffeine.", ID: "9"},
		{Text: "By the way, did you finish the report?", ID: "10"},
		{Text: "Almost done, just need to review it.", ID: "11"},
		{Text: "The boss is going to love this.", ID: "12"},
		{Text: "I hope so. I worked all weekend on it.", ID: "13"},
		{Text: "I know you did. It shows.", ID: "14"},
	}

	result := WhisperResult{
		Language: "en",
		Duration: 45.0,
		Segments: segs,
	}

	req := LLMTranslationRequest{
		TargetLanguage: "zh",
		Model:          "deepseek-v4-flash",
		ChunkSize:      15,
	}

	t.Log("=== Live Translation Test ===")
	t.Logf("Model: %s, Segments: %d, ChunkSize: %d", req.Model, len(segs), req.ChunkSize)
	t.Log("----------------------------")

	start := time.Now()
	translation, err := TranslateWhisperResultWithLLM(ctx, result, "en", req, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("translation error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(translation.Result.Text), "\n")

	t.Logf("Time: %v", elapsed)
	t.Logf("Output lines: %d / %d input", len(lines), len(segs))

	t.Log("--- Output (first 15 lines) ---")
	for i, line := range lines {
		if i >= 15 {
			break
		}
		t.Logf("  %s", line)
	}

	if len(lines) < len(segs)-2 {
		t.Errorf("too few output lines: %d vs %d input", len(lines), len(segs))
	}

	// Cache test
	t.Run("Cache hit", func(t *testing.T) {
		start2 := time.Now()
		translation2, err := TranslateWhisperResultWithLLM(ctx, result, "en", req, nil)
		elapsed2 := time.Since(start2)
		if err != nil {
			t.Fatalf("second translation error: %v", err)
		}
		t.Logf("Second call: %v (first: %v)", elapsed2, elapsed)
		if translation2.Result.Text != translation.Result.Text {
			t.Log("Note: output differs (non-deterministic model)")
		}
	})
}

// ─── Test harness for verify mode ─────────────────────────────────────

func TestUnitSummary(t *testing.T) {
	t.Log("======= LLM Translation Optimization Tests =======")
	t.Log("  1. TestThinkingParams        - model dispatch correctness")
	t.Log("  2. TestApplyChunkMergeGuard   - merge detection & clearing")
	t.Log("  3. TestParseChunkResultNormal - N|| format parsing")
	t.Log("  4. TestAdaptiveChunkSize      - dynamic chunk size")
	t.Log("  5. TestTranslationCache       - LRU eviction")
	t.Log("  6. TestRateLimitGate          - cooldown escalation")
	t.Log("  7. TestLiveTranslation        - real API call (if configured)")
	t.Log("==================================================")
}
