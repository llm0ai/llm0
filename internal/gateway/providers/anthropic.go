package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/llm0ai/llm0/internal/shared/config"
	"github.com/sashabaranov/go-openai"
)

// AnthropicProvider handles Anthropic Claude API requests
type AnthropicProvider struct {
	apiKey     string
	httpClient *http.Client
	cfg        *config.Config
}

// AnthropicMessage represents a message in the Anthropic format
type AnthropicMessage struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"` // Text content
}

// AnthropicRequest represents a request to Anthropic's Messages API
type AnthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []AnthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float32           `json:"temperature,omitempty"`
	TopP        *float32           `json:"top_p,omitempty"`
	TopK        *int               `json:"top_k,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	System      string             `json:"system,omitempty"` // System prompt (optional)
}

// AnthropicResponse represents a response from Anthropic's Messages API
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // "message"
	Role         string                  `json:"role"` // "assistant"
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`   // "end_turn", "max_tokens", "stop_sequence"
	StopSequence *string                 `json:"stop_sequence"` // null or string
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicContentBlock represents a content block in Anthropic's response
type AnthropicContentBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// AnthropicUsage represents token usage from Anthropic
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicErrorDetail is the nested error object inside Anthropic's error response.
type AnthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// AnthropicError represents an error response from Anthropic API.
// Anthropic wraps the actual error under the "error" key:
//
//	{ "type": "error", "error": { "type": "...", "message": "..." } }
type AnthropicError struct {
	Type   string               `json:"type"`
	Detail AnthropicErrorDetail `json:"error"`
}

// Error implements the error interface for AnthropicError
func (e *AnthropicError) Error() string {
	return e.Detail.Message
}

// Anthropic Streaming Event Types
type AnthropicStreamEvent struct {
	Type         string                  `json:"type"`
	Message      *AnthropicStreamMessage `json:"message,omitempty"`
	Index        int                     `json:"index,omitempty"`
	ContentBlock *AnthropicContentBlock  `json:"content_block,omitempty"`
	Delta        *AnthropicStreamDelta   `json:"delta,omitempty"`
	Usage        *AnthropicStreamUsage   `json:"usage,omitempty"`
}

type AnthropicStreamMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

type AnthropicStreamDelta struct {
	Type       string  `json:"type"`
	Text       string  `json:"text,omitempty"`
	StopReason *string `json:"stop_reason,omitempty"`
}

type AnthropicStreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicStreamReader wraps the HTTP response for streaming
type AnthropicStreamReader struct {
	reader *bufio.Reader
	resp   *http.Response
}

// NewAnthropicProvider creates a new Anthropic provider
func NewAnthropicProvider(cfg *config.Config) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: cfg.AnthropicAPIKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second, // Claude can take longer for complex responses
		},
		cfg: cfg,
	}
}

// NewAnthropicProviderWithKey creates a provider with a custom API key (for BYOK)
func NewAnthropicProviderWithKey(apiKey string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// ChatCompletion makes a chat completion request to Anthropic
func (p *AnthropicProvider) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	startTime := time.Now()

	// Convert OpenAI-style messages to Anthropic format
	anthropicReq, systemPrompt := p.convertRequest(req)

	// Set system prompt if extracted
	if systemPrompt != "" {
		anthropicReq.System = systemPrompt
	}

	// Make HTTP request to Anthropic API
	respBody, err := p.makeRequest(ctx, anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("Anthropic API error: %w", err)
	}

	// Parse response
	var anthropicResp AnthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to parse Anthropic response: %w", err)
	}

	latencyMs := int(time.Since(startTime).Milliseconds())

	// Convert Anthropic response to our standard format
	return p.convertResponse(anthropicResp, latencyMs), nil
}

// convertRequest converts our standard ChatRequest to Anthropic's format
func (p *AnthropicProvider) convertRequest(req ChatRequest) (AnthropicRequest, string) {
	anthropicReq := AnthropicRequest{
		Model:       req.Model,
		Messages:    []AnthropicMessage{},
		MaxTokens:   4096, // Default max tokens (Anthropic requires this)
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}

	// Override max_tokens if specified
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		anthropicReq.MaxTokens = *req.MaxTokens
	}

	var systemPrompt string

	// Convert messages (extract system prompt, convert rest)
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			// Anthropic uses a separate "system" field
			systemPrompt = msg.Content
		case "user", "assistant":
			anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{
				Role:    msg.Role,
				Content: msg.Content,
			})
		}
	}

	return anthropicReq, systemPrompt
}

// convertResponse converts Anthropic's response to our standard format
func (p *AnthropicProvider) convertResponse(resp AnthropicResponse, latencyMs int) *ChatResponse {
	// Extract text from content blocks
	var content string
	for _, block := range resp.Content {
		if block.Type == "text" {
			content += block.Text
		}
	}

	// Build OpenAI-compatible response
	return &ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: content,
				},
				FinishReason: openai.FinishReason(p.convertFinishReason(resp.StopReason)),
			},
		},
		Usage: openai.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
		LatencyMs: latencyMs,
	}
}

// convertFinishReason converts Anthropic's stop_reason to OpenAI's finish_reason
func (p *AnthropicProvider) convertFinishReason(stopReason string) string {
	switch stopReason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return stopReason
	}
}

// makeRequest makes an HTTP request to Anthropic API
func (p *AnthropicProvider) makeRequest(ctx context.Context, req AnthropicRequest) ([]byte, error) {
	// Marshal request body
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers (Anthropic-specific)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01") // Required API version header

	// Make request
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for errors
	if httpResp.StatusCode != http.StatusOK {
		var apiError AnthropicError
		if err := json.Unmarshal(respBody, &apiError); err != nil || apiError.Detail.Message == "" {
			return nil, fmt.Errorf("Anthropic API error (status %d): %s", httpResp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("Anthropic API error (status %d): %s", httpResp.StatusCode, apiError.Detail.Message)
	}

	return respBody, nil
}

// ChatCompletionStream makes a streaming chat completion request to Anthropic
func (p *AnthropicProvider) ChatCompletionStream(ctx context.Context, req ChatRequest) (*AnthropicStreamReader, error) {
	// Convert OpenAI-style messages to Anthropic format
	anthropicReq, systemPrompt := p.convertRequest(req)
	anthropicReq.Stream = true // Enable streaming

	// Set system prompt if extracted
	if systemPrompt != "" {
		anthropicReq.System = systemPrompt
	}

	// Marshal request body
	reqBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers (Anthropic-specific)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	// Make request
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}

	// Check for errors
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		respBody, _ := io.ReadAll(httpResp.Body)
		var apiError AnthropicError
		if err := json.Unmarshal(respBody, &apiError); err != nil || apiError.Detail.Message == "" {
			return nil, fmt.Errorf("Anthropic API error (status %d): %s", httpResp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("Anthropic API error (status %d): %s", httpResp.StatusCode, apiError.Detail.Message)
	}

	return &AnthropicStreamReader{
		reader: bufio.NewReader(httpResp.Body),
		resp:   httpResp,
	}, nil
}

// Recv reads the next streaming chunk from Anthropic
func (r *AnthropicStreamReader) Recv() (openai.ChatCompletionStreamResponse, error) {
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return openai.ChatCompletionStreamResponse{}, err
		}

		line = strings.TrimSpace(line)

		// Skip empty lines
		if line == "" {
			continue
		}

		// Anthropic sends "event: <type>" and "data: <json>"
		if strings.HasPrefix(line, "event:") {
			// Read event type (we'll get the data on next iteration)
			continue
		}

		if strings.HasPrefix(line, "data:") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

			// Parse the JSON event
			var event AnthropicStreamEvent
			if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
				return openai.ChatCompletionStreamResponse{}, fmt.Errorf("failed to parse stream event: %w", err)
			}

			// Convert Anthropic event to our unified format
			chunk := r.convertEventToChunk(event)
			if chunk != nil {
				return *chunk, nil
			}
			// If chunk is nil, continue to next event
		}
	}
}

// Close closes the stream
func (r *AnthropicStreamReader) Close() error {
	if r.resp != nil && r.resp.Body != nil {
		return r.resp.Body.Close()
	}
	return nil
}

// convertEventToChunk converts Anthropic streaming events to our unified chunk format
func (r *AnthropicStreamReader) convertEventToChunk(event AnthropicStreamEvent) *openai.ChatCompletionStreamResponse {
	chunk := &openai.ChatCompletionStreamResponse{
		Object:  "chat.completion.chunk",
		Choices: []openai.ChatCompletionStreamChoice{},
	}

	switch event.Type {
	case "message_start":
		// First event with message metadata
		if event.Message != nil {
			chunk.ID = event.Message.ID
			chunk.Model = event.Message.Model
			chunk.Created = time.Now().Unix()
			chunk.Choices = []openai.ChatCompletionStreamChoice{
				{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Role: "assistant",
					},
				},
			}
			// Store initial usage if available
			if event.Message.Usage.InputTokens > 0 {
				chunk.Usage = &openai.Usage{
					PromptTokens: event.Message.Usage.InputTokens,
				}
			}
			return chunk
		}

	case "content_block_start":
		// New content block starting (skip for now, we'll get the delta)
		return nil

	case "content_block_delta":
		// Actual content delta - this is what we want
		if event.Delta != nil && event.Delta.Text != "" {
			chunk.Choices = []openai.ChatCompletionStreamChoice{
				{
					Index: event.Index,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Content: event.Delta.Text,
					},
				},
			}
			return chunk
		}

	case "content_block_stop":
		// Content block ended (skip)
		return nil

	case "message_delta":
		// Final delta with stop reason and usage
		if event.Delta != nil && event.Delta.StopReason != nil {
			finishReason := convertAnthropicFinishReason(*event.Delta.StopReason)
			chunk.Choices = []openai.ChatCompletionStreamChoice{
				{
					Index:        0,
					Delta:        openai.ChatCompletionStreamChoiceDelta{},
					FinishReason: openai.FinishReason(finishReason),
				},
			}
		}
		// Add usage if available
		if event.Usage != nil {
			chunk.Usage = &openai.Usage{
				PromptTokens:     event.Usage.InputTokens,
				CompletionTokens: event.Usage.OutputTokens,
				TotalTokens:      event.Usage.InputTokens + event.Usage.OutputTokens,
			}
		}
		return chunk

	case "message_stop":
		// Stream ended - return EOF
		return nil

	case "ping":
		// Keepalive ping (skip)
		return nil

	case "error":
		// Error event
		return nil
	}

	return nil
}

// convertAnthropicFinishReason converts Anthropic's stop_reason to OpenAI's finish_reason
func convertAnthropicFinishReason(stopReason string) string {
	switch stopReason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return stopReason
	}
}

// ValidateModel claims Anthropic's model namespace by prefix match.
//
// We deliberately avoid a hard-coded allowlist here. Anthropic ships new
// Claude variants regularly (3, 3.5, 4, 4.5, 4.6, 4.7, opus/sonnet/haiku
// sub-tiers, dated vs alias names, …) and users can register pricing at
// runtime via scripts/manage_models.sh. Gating on a static list would force
// a rebuild for every new release.
//
// If the upstream API doesn't recognize the model, it returns a 404 which
// the failover executor treats as retriable.
func (p *AnthropicProvider) ValidateModel(model string) bool {
	return strings.HasPrefix(model, "claude-")
}

// GetProviderName returns the provider name
func (p *AnthropicProvider) GetProviderName() string {
	return "anthropic"
}
