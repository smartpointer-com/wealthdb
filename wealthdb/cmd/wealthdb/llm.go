package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
)

// The LLM plumbing shared by every overlay command that asks a model
// for a verdict and stores it in gold: `resolve-symbols` (tickers,
// per-source) and `categorize` (spend categories, per merchant).
//
// The two commands share the transport, the response hygiene, and the
// config check; they share nothing else. Each owns its own prompt, its
// own CSV columns, and its own validation gauntlet, because what makes
// a response wrong is entirely specific to what was asked.

// llmCall is the seam a retry loop calls through. Production passes a
// closure over callLLM; a test passes a scripted responder, so a
// loop's behaviour — retry with feedback, union across attempts, the
// validation gauntlet — is testable without a model on the other end.
type llmCall func(ctx context.Context, system, user string) (string, error)

// modelCaller adapts a configured endpoint to the llmCall seam.
func modelCaller(cfg *config.ModelConfig) llmCall {
	return func(ctx context.Context, system, user string) (string, error) {
		return callLLM(ctx, cfg, system, user)
	}
}

// validateModelConfig checks the required model fields are set. The
// shape of baseUrl is not enforced (net/http surfaces the URL error
// if it is malformed) but every required piece needs to be non-empty.
//
// keyPrefix roots every message in the config key that actually holds
// the block — "spending.categorization.model",
// "symbol_resolution.model" — so the message names something greppable
// in the config file, and a deployment with both blocks can tell which
// one is broken.
func validateModelConfig(keyPrefix string, m *config.ModelConfig) error {
	switch {
	case m.BaseURL == "":
		return fmt.Errorf("%s.baseUrl is required", keyPrefix)
	case m.Name == "":
		return fmt.Errorf("%s.name is required", keyPrefix)
	case m.API != "" && m.API != "openai-completions":
		return fmt.Errorf("%s.api %q not supported; only 'openai-completions' is wired today", keyPrefix, m.API)
	}
	return nil
}

// invalidRow is a row the model emitted that failed validation.
// Carried into the next retry's prompt as targeted feedback.
type invalidRow struct {
	Raw    []string // the model's emitted CSV cells
	Reason string
}

// splitBatches cuts a model pass's candidates into consecutive runs of
// at most size, one model call each. A backlog sent whole in a single
// call dies on callLLM's five-minute ceiling: no local model answers a
// prompt that size inside it, and none should be asked to.
func splitBatches[T any](items []T, size int) [][]T {
	if size < 1 {
		size = 1
	}
	var out [][]T
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		out = append(out, items[start:end])
	}
	return out
}

// openAIRequest mirrors the chat-completions JSON body. Only the
// fields we actually set; the server tolerates extras.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
	} `json:"error,omitempty"`
}

// callLLM POSTs to {baseUrl}/chat/completions and returns the
// first choice's message content. Bearer-auth with apiKey when set.
// Surfaces non-2xx status as an error including the body for
// debugging. Temperature is pinned to 0: these are lookups, and a
// sampled answer would make a re-run disagree with itself.
func callLLM(ctx context.Context, cfg *config.ModelConfig, system, user string) (string, error) {
	body, err := json.Marshal(openAIRequest{
		Model: cfg.Name,
		Messages: []openAIMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	// MLX local serves tend to be slow on long prompts — give it
	// a generous per-call ceiling. It can be interrupted (ctrl-C) if it
	// wedges. (No background goroutines to clean up; this is a
	// straight blocking call.)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, truncate(string(respBody), 500))
	}
	var parsed openAIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(respBody), 500))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("API error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("API returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// thinkBlockRe matches deepseek-style <think>...</think> reasoning
// blocks. (?s) lets . span newlines and .*? stays non-greedy so
// stacked / interleaved blocks each strip individually.
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinkingBlocks removes deepseek-style <think>...</think>
// reasoning blocks from the response. A reasoning model emits them
// inline ahead of the CSV body, so the strip runs unconditionally
// on every response as parse hygiene — no config field gates it.
// We strip non-greedily to handle stacked / interleaved blocks.
func stripThinkingBlocks(s string) string {
	return strings.TrimSpace(thinkBlockRe.ReplaceAllString(s, ""))
}

// stripCodeFences removes ```csv ... ``` and ``` ... ``` wrappers
// that chat models love to add even when told to emit raw CSV.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (including an optional language tag).
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		return ""
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
