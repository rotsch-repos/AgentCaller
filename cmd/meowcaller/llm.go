package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// LLMConfig points at the local vLLM endpoint that generates the spoken answer.
// The endpoint speaks the OpenAI-compatible /v1/chat/completions protocol and needs
// no API key — the whole chain stays on this host.
type LLMConfig struct {
	// Endpoint is the base URL, e.g. http://127.0.0.1:8002.
	Endpoint string
	// ModelID is the model to request.
	ModelID string
	// Prompt is the system instruction.
	Prompt string
	// Timeout bounds the completion request.
	Timeout time.Duration
	// MaxTokens caps the answer length.
	MaxTokens int
}

// DefaultLLMPrompt is the butler-style answer instruction.
const DefaultLLMPrompt = "Du bist ein britischer Butler. Antworte auf die Anmerkung deines Herrn in 1–2 kurzen Sätzen, die gut klingen, wenn vorgelesen. Kein Markdown, keine Anführungszeichen, maximal 40 Wörter."

func defaultLLMConfig() LLMConfig {
	return LLMConfig{
		Endpoint:  "http://127.0.0.1:8002",
		ModelID:   "qwen3.8-27b",
		Prompt:    DefaultLLMPrompt,
		Timeout:   30 * time.Second,
		MaxTokens: 120,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float32       `json:"temperature"`
	Stream      bool          `json:"stream"`
	// ChatTemplateKwargs disables the reasoning trace on thinking models
	// (Qwen3 "abliterated" builds put every token into `reasoning` and leave
	// `content` null otherwise).
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// generateReply asks the local LLM for the spoken answer to a transcript.
func generateReply(ctx context.Context, cfg LLMConfig, transcript string) (string, error) {
	if cfg.Endpoint == "" {
		cfg = defaultLLMConfig()
	}
	body, err := json.Marshal(chatRequest{
		Model: cfg.ModelID,
		Messages: []chatMessage{
			{Role: "system", Content: cfg.Prompt},
			{Role: "user", Content: "Transkript: " + transcript},
		},
		MaxTokens:          cfg.MaxTokens,
		Temperature:        0.3,
		Stream:             false,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return "", fmt.Errorf("encode LLM request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Endpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM request failed (is %s running?): %w", cfg.Endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body chatResponse
		_ = json.NewDecoder(resp.Body).Decode(&body)
		msg := ""
		if body.Error != nil {
			msg = body.Error.Message
		}
		return "", fmt.Errorf("LLM endpoint %s returned %s: %s", cfg.Endpoint, resp.Status, msg)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode LLM response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	answer := strings.TrimSpace(out.Choices[0].Message.Content)
	answer = stripQuotes(answer)
	if answer == "" {
		return "", fmt.Errorf("LLM returned an empty answer")
	}
	return answer, nil
}

// stripQuotes removes surrounding quotes and trailing markdown the model may add.
func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []struct{ a, b string }{{"«", "»"}, {"“", "”"}, {"„", "“"}, {"\"", "\""}, {"‘", "’"}} {
		if strings.HasPrefix(s, q.a) && strings.HasSuffix(s, q.b) && len(s) >= len(q.a)+len(q.b) {
			s = s[len(q.a) : len(s)-len(q.b)]
		}
	}
	s = strings.TrimLeft(s, " \t\n\r")
	if strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "«") {
		s = s[1:]
	}
	return strings.TrimSpace(s)
}

var _ = zerolog.Nop // keep zerolog referenced for future logging in this file
