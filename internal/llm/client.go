// Package llm implements a minimal OpenAI-compatible chat client. It has no SDK
// dependency on purpose: the wire contract is small and keeping it visible makes
// the retry/timeout/error behaviour easy to audit.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one chat turn in the OpenAI wire format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client calls a /chat/completions endpoint.
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	maxTokens   int
	temperature float64
	http        *http.Client
}

// New builds a client. Callers must supply the API key from the environment.
func New(baseURL, apiKey, model string, timeout time.Duration, maxTokens int, temperature float64) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		http:        &http.Client{Timeout: timeout},
	}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	// No omitempty: temperature=0 is a meaningful setting (deterministic
	// sampling). Dropping it silently handed the decision to the server default.
	Temperature float64 `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// EmptyCompletionError means the model spent its whole budget without emitting
// content (common with reasoning models). It is retryable with a larger budget.
type EmptyCompletionError struct {
	Reasoned bool
}

func (e *EmptyCompletionError) Error() string {
	if e.Reasoned {
		return "llm: empty completion content (reasoning consumed the token budget; raise LLM_MAX_TOKENS)"
	}
	return "llm: empty completion content"
}

// StatusError is a non-2xx upstream answer. 429 and 5xx are retried once with
// exponential backoff; everything else (auth, bad request) fails immediately.
type StatusError struct {
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm: upstream status %d: %s", e.Code, e.Body)
}

func (e *StatusError) retryable() bool {
	switch e.Code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// MaxAttempts is the hard ceiling for one Chat call: the first request plus at
// most one retry for an empty reasoning completion and one for 429/5xx. The
// service uses it to size the overall deadline (per-attempt timeout * attempts)
// so retries never consume the first attempt's budget.
const MaxAttempts = 3

const (
	maxRetryTokens = 16000
	backoffBase    = 300 * time.Millisecond
	maxRetryAfter  = 5 * time.Second
)

// Chat sends a system/user pair and returns the assistant text.
//
// Retries are separate from the per-attempt timeout: http.Client.Timeout bounds
// each attempt, so a retry gets a fresh budget instead of inheriting an expired
// one. An empty completion is retried with a doubled token budget (capped);
// 429/5xx are retried once after an exponential backoff, honouring Retry-After.
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("llm: empty api key")
	}
	effort := c.maxTokens
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		content, err := c.chatOnce(ctx, system, user, effort)
		if err == nil {
			return content, nil
		}
		lastErr = err

		var empty *EmptyCompletionError
		if errors.As(err, &empty) && effort < maxRetryTokens && attempt < MaxAttempts {
			effort *= 2
			if effort > maxRetryTokens {
				effort = maxRetryTokens
			}
			continue
		}

		var status *StatusError
		if errors.As(err, &status) && status.retryable() && attempt < MaxAttempts {
			if err := sleepBackoff(ctx, attempt, status.RetryAfter); err != nil {
				return "", err
			}
			continue
		}
		return "", err
	}
	return "", lastErr
}

// sleepBackoff waits base*2^(attempt-1), or Retry-After when the upstream
// supplied one (capped so a hostile header cannot stall a triage).
func sleepBackoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	wait := retryAfter
	if wait <= 0 {
		wait = backoffBase << (attempt - 1)
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) chatOnce(ctx context.Context, system, user string, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens:   maxTokens,
		Temperature: c.temperature,
	})
	if err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &StatusError{
			Code:       resp.StatusCode,
			Body:       snippet(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: decode response: %w (body: %s)", err, snippet(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: upstream error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: response contained no choices")
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return "", &EmptyCompletionError{Reasoned: parsed.Choices[0].Message.ReasoningContent != ""}
	}
	return content, nil
}

func snippet(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// parseRetryAfter understands the delta-seconds form (the form LLM gateways
// send). HTTP-date form is ignored and falls back to exponential backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
