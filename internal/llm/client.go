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
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
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

const maxRetryTokens = 16000

// Chat sends a system/user pair and returns the assistant text. If a reasoning
// model exhausts the token budget before producing content, it retries once with
// a doubled budget (capped) instead of failing the whole triage.
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("llm: empty api key")
	}
	content, err := c.chatOnce(ctx, system, user, c.maxTokens)
	if err == nil {
		return content, nil
	}
	var empty *EmptyCompletionError
	if errors.As(err, &empty) && c.maxTokens < maxRetryTokens {
		next := c.maxTokens * 2
		if next > maxRetryTokens {
			next = maxRetryTokens
		}
		return c.chatOnce(ctx, system, user, next)
	}
	return "", err
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
		return "", fmt.Errorf("llm: upstream status %d: %s", resp.StatusCode, snippet(raw))
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
