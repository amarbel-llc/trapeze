// Package openrouter is a minimal OpenAI-compatible chat-completions client,
// used by trapeze's headless XMPP agent mode to talk to OpenRouter (or any
// OpenAI-compatible endpoint). It is deliberately tiny: pure conversational
// completion, no tools, no streaming — enough to back a chat-only agent.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is OpenRouter's OpenAI-compatible API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client calls an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client

	// Title/Referer populate OpenRouter's optional attribution headers
	// (HTTP-Referer / X-Title); harmless against other OpenAI-compatible APIs.
	Title   string
	Referer string
}

// New builds a Client with sane defaults. baseURL defaults to DefaultBaseURL.
func New(baseURL, apiKey, model string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Title:   "trapeze",
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends the conversation and returns the assistant's reply text.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("openrouter: API key is required")
	}
	if c.Model == "" {
		return "", fmt.Errorf("openrouter: model is required")
	}

	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages})
	if err != nil {
		return "", fmt.Errorf("openrouter: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openrouter: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if c.Referer != "" {
		req.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		req.Header.Set("X-Title", c.Title)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter: request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("openrouter: read response: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("openrouter: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("openrouter: api error (status %d): %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("openrouter: api error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openrouter: empty choices in response")
	}
	return parsed.Choices[0].Message.Content, nil
}
