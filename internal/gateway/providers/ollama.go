package providers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/llm0ai/llm0/internal/shared/config"
	"github.com/sashabaranov/go-openai"
)

// OllamaProvider routes requests to a locally-running Ollama instance via its
// OpenAI-compatible API endpoint (GET /v1/chat/completions).
type OllamaProvider struct {
	client  *openai.Client
	cfg     *config.Config
	baseURL string
}

// NewOllamaProvider creates a provider backed by the Ollama server at cfg.OllamaBaseURL.
// Returns nil when OllamaBaseURL is empty so callers can skip registration cleanly.
func NewOllamaProvider(cfg *config.Config) *OllamaProvider {
	if cfg.OllamaBaseURL == "" {
		return nil
	}

	clientCfg := openai.DefaultConfig("ollama") // key is ignored by Ollama
	clientCfg.BaseURL = cfg.OllamaBaseURL
	clientCfg.HTTPClient = &http.Client{Timeout: 120 * time.Second} // local can be slow on first run

	return &OllamaProvider{
		client:  openai.NewClientWithConfig(clientCfg),
		cfg:     cfg,
		baseURL: cfg.OllamaBaseURL,
	}
}

// ChatCompletion sends a blocking chat completion request to Ollama.
func (p *OllamaProvider) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	startTime := time.Now()

	ollamaReq := openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: req.Messages,
	}
	if req.Temperature != nil {
		ollamaReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		ollamaReq.MaxTokens = *req.MaxTokens
	}
	if req.TopP != nil {
		ollamaReq.TopP = *req.TopP
	}

	resp, err := p.client.CreateChatCompletion(ctx, ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama API error: %w", err)
	}

	return &ChatResponse{
		ID:        resp.ID,
		Object:    resp.Object,
		Created:   resp.Created,
		Model:     resp.Model,
		Choices:   resp.Choices,
		Usage:     resp.Usage,
		LatencyMs: int(time.Since(startTime).Milliseconds()),
	}, nil
}

// ChatCompletionStream creates a streaming chat completion request to Ollama.
func (p *OllamaProvider) ChatCompletionStream(ctx context.Context, req ChatRequest) (*openai.ChatCompletionStream, error) {
	ollamaReq := openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: req.Messages,
		Stream:   true,
	}
	if req.Temperature != nil {
		ollamaReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		ollamaReq.MaxTokens = *req.MaxTokens
	}
	if req.TopP != nil {
		ollamaReq.TopP = *req.TopP
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama streaming API error: %w", err)
	}
	return stream, nil
}

// ValidateModel always returns true — Ollama accepts any model the user has pulled.
// Model availability errors surface naturally at request time.
func (p *OllamaProvider) ValidateModel(_ string) bool {
	return true
}

// ListModels returns the models currently available in the Ollama instance.
func (p *OllamaProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("Ollama list models error: %w", err)
	}
	ids := make([]string, 0, len(models.Models))
	for _, m := range models.Models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// BaseURL returns the configured Ollama base URL (useful for health checks).
func (p *OllamaProvider) BaseURL() string {
	return p.baseURL
}
