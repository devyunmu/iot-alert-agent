package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatSuccess(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "  hello agent  "}},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1/", "sk-test", "test-model", 5*time.Second, 256, 0.3)
	out, err := c.Chat(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "hello agent" {
		t.Errorf("content = %q", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q (base URL must be trimmed and joined correctly)", gotPath)
	}
	if gotBody.Model != "test-model" || gotBody.MaxTokens != 256 || gotBody.Temperature != 0.3 {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	if len(gotBody.Messages) != 2 || gotBody.Messages[0].Role != "system" {
		t.Errorf("unexpected messages: %+v", gotBody.Messages)
	}
	if c.Model() != "test-model" {
		t.Errorf("Model() = %q", c.Model())
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"upstream error", 401, `{"error":{"message":"invalid token"}}`, "401"},
		{"malformed json", 200, `{not json`, "decode response"},
		{"no choices", 200, `{"choices":[]}`, "no choices"},
		{"empty content", 200, `{"choices":[{"message":{"content":"   "}}]}`, "empty completion"},
		{"error field", 200, `{"error":{"message":"quota exceeded"}}`, "quota exceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := New(srv.URL, "sk-test", "m", 2*time.Second, 128, 0.2)
			_, err := c.Chat(context.Background(), "s", "u")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestChatEmptyKey(t *testing.T) {
	c := New("http://127.0.0.1:1/v1", "", "m", time.Second, 10, 0)
	if _, err := c.Chat(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "api key") {
		t.Fatalf("err = %v, want empty api key error", err)
	}
}

func TestChatRetriesOnEmptyReasoning(t *testing.T) {
	var calls int
	var budgets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		budgets = append(budgets, body.MaxTokens)
		calls++
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{
					"content": "", "reasoning_content": "long internal monologue",
				}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "recovered"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "reasoner", 2*time.Second, 1000, 0.2)
	out, err := c.Chat(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "recovered" || calls != 2 {
		t.Fatalf("out=%q calls=%d", out, calls)
	}
	if len(budgets) != 2 || budgets[0] != 1000 || budgets[1] != 2000 {
		t.Errorf("token budgets = %v, want [1000 2000]", budgets)
	}
}

func TestChatTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "m", 30*time.Millisecond, 10, 0)
	if _, err := c.Chat(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected timeout error")
	}
}
