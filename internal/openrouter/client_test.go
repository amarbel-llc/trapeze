package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteSuccess(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message Message `json:"message"`
			}{{Message: Message{Role: "assistant", Content: "hi there"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "openai/gpt-4o-mini")
	reply, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply != "hi there" {
		t.Errorf("reply = %q, want %q", reply, "hi there")
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"openai/gpt-4o-mini"`) {
		t.Errorf("request body missing model: %s", gotBody)
	}
}

func TestCompleteAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected rate-limited error, got %v", err)
	}
}

func TestCompleteValidation(t *testing.T) {
	if _, err := New("", "", "m").Complete(context.Background(), nil); err == nil {
		t.Error("expected error when API key missing")
	}
	if _, err := New("", "k", "").Complete(context.Background(), nil); err == nil {
		t.Error("expected error when model missing")
	}
}

func TestNewDefaultsBaseURL(t *testing.T) {
	if c := New("", "k", "m"); c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want default", c.BaseURL)
	}
	if c := New("https://x/", "k", "m"); c.BaseURL != "https://x" {
		t.Errorf("trailing slash not trimmed: %q", c.BaseURL)
	}
}
