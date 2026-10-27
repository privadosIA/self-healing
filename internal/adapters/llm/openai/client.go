package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxResponseBytes = 10 << 20
	// temperature is low on purpose: a patch should be reproducible, not creative.
	temperature = 0.1
)

// ErrTruncated is returned when the model stopped because it hit its token limit.
var ErrTruncated = errors.New("openai: response truncated by the token limit")

// Config configures a Client.
type Config struct {
	// BaseURL is the API root including the version, for example
	// http://llm.example.com/v1.
	BaseURL string
	// APIKey is sent as a bearer token when not empty.
	APIKey string
	// Model is the model name or alias.
	Model string
	// Client performs the requests; use safehttp.NewClient so the allowlist applies.
	Client *http.Client
}

// Completion is the model's answer and its token usage.
type Completion struct {
	Content   string
	TokensIn  int
	TokensOut int
}

// Client talks to an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
}

// NewClient validates cfg and returns a Client.
func NewClient(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("openai: BaseURL must be an http(s) URL with a host")
	}
	if cfg.Model == "" {
		return nil, errors.New("openai: Model is required")
	}
	if cfg.Client == nil {
		return nil, errors.New("openai: Client is required")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/chat/completions"
	return &Client{endpoint: base.String(), apiKey: cfg.APIKey, model: cfg.Model, http: cfg.Client}, nil
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []message         `json:"messages"`
	Temperature    float64           `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat sends a system and a user message and asks for a JSON object back.
// Error messages never include the request or the response body, which may
// hold source code.
func (c *Client) Chat(ctx context.Context, system, user string) (Completion, error) {
	payload, err := json.Marshal(chatRequest{
		Model:          c.model,
		Messages:       []message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature:    temperature,
		ResponseFormat: map[string]string{"type": "json_object"},
	})
	if err != nil {
		return Completion{}, fmt.Errorf("openai: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Completion{}, fmt.Errorf("openai: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Completion{}, fmt.Errorf("openai: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Completion{}, fmt.Errorf("openai: server returned HTTP %d", resp.StatusCode)
	}

	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return Completion{}, fmt.Errorf("openai: invalid response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Completion{}, errors.New("openai: response has no choices")
	}
	choice := out.Choices[0]
	if choice.FinishReason == "length" {
		return Completion{}, ErrTruncated
	}
	return Completion{
		Content:   choice.Message.Content,
		TokensIn:  out.Usage.PromptTokens,
		TokensOut: out.Usage.CompletionTokens,
	}, nil
}
