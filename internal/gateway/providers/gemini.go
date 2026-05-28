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

// GeminiProvider handles Google Gemini API requests
//
// API: Google AI Studio (generativelanguage.googleapis.com)
// Auth: Simple API key (NOT Vertex AI OAuth)
// Get Key: https://ai.google.dev/
//
// Note: This uses the Google AI Studio API, not Vertex AI.
// For Vertex AI support (enterprise), see GEMINI_API_OPTIONS.md
type GeminiProvider struct {
	apiKey     string
	httpClient *http.Client
	cfg        *config.Config
}

// GeminiRequest represents a request to Gemini's generateContent API
type GeminiRequest struct {
	Contents         []GeminiContent         `json:"contents"`
	GenerationConfig *GeminiGenerationConfig `json:"generationConfig,omitempty"`
}

// GeminiContent represents content in Gemini format
type GeminiContent struct {
	Role  string       `json:"role"` // "user" or "model"
	Parts []GeminiPart `json:"parts"`
}

// GeminiPart represents a part of the content
type GeminiPart struct {
	Text string `json:"text"`
}

// GeminiGenerationConfig represents generation parameters
type GeminiGenerationConfig struct {
	Temperature     *float32 `json:"temperature,omitempty"`
	TopP            *float32 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// GeminiResponse represents a response from Gemini API
type GeminiResponse struct {
	Candidates    []GeminiCandidate `json:"candidates"`
	UsageMetadata GeminiUsage       `json:"usageMetadata"`
}

// GeminiCandidate represents a candidate response
type GeminiCandidate struct {
	Content       GeminiContent `json:"content"`
	FinishReason  string        `json:"finishReason"`
	Index         int           `json:"index"`
	SafetyRatings []interface{} `json:"safetyRatings,omitempty"`
}

// GeminiUsage represents token usage
type GeminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// GeminiError represents an error response from Gemini API
type GeminiError struct {
	ErrorDetails struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Error implements the error interface for GeminiError
func (e *GeminiError) Error() string {
	return e.ErrorDetails.Message
}

// GeminiStreamReader wraps the HTTP response for streaming
type GeminiStreamReader struct {
	reader *bufio.Reader
	resp   *http.Response
	model  string
}

// NewGeminiProvider creates a new Gemini provider
func NewGeminiProvider(cfg *config.Config) *GeminiProvider {
	return &GeminiProvider{
		apiKey: cfg.GeminiAPIKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		cfg: cfg,
	}
}

// NewGeminiProviderWithKey creates a provider with a custom API key (for BYOK)
func NewGeminiProviderWithKey(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// ChatCompletion makes a chat completion request to Gemini
func (p *GeminiProvider) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	startTime := time.Now()

	// Convert OpenAI-style messages to Gemini format
	geminiReq := p.convertRequest(req)

	// Build API URL with model name
	// Using Google AI Studio API (NOT Vertex AI)
	// Format: https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?key={apiKey}
	// Docs: https://ai.google.dev/gemini-api/docs
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		req.Model, p.apiKey)

	// Marshal request
	reqBody, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Make request
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Gemini API request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		var geminiErr GeminiError
		if err := json.Unmarshal(body, &geminiErr); err == nil && geminiErr.ErrorDetails.Message != "" {
			return nil, fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, geminiErr.ErrorDetails.Message)
		}
		return nil, fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, string(body))
	}

	// Parse response
	var geminiResp GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Convert to OpenAI-compatible response
	openaiResp := p.convertResponse(geminiResp, req.Model)
	openaiResp.LatencyMs = int(time.Since(startTime).Milliseconds())

	return openaiResp, nil
}

// convertRequest converts OpenAI format to Gemini format
func (p *GeminiProvider) convertRequest(req ChatRequest) GeminiRequest {
	geminiReq := GeminiRequest{
		Contents: make([]GeminiContent, 0),
	}

	// Convert messages
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "assistant" {
			role = "model" // Gemini uses "model" instead of "assistant"
		}
		if role == "system" {
			// Gemini doesn't have a system role, prepend as user message
			role = "user"
		}

		content := GeminiContent{
			Role: role,
			Parts: []GeminiPart{
				{Text: msg.Content},
			},
		}
		geminiReq.Contents = append(geminiReq.Contents, content)
	}

	// Add generation config
	if req.Temperature != nil || req.MaxTokens != nil || req.TopP != nil {
		geminiReq.GenerationConfig = &GeminiGenerationConfig{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
		}
	}

	return geminiReq
}

// convertResponse converts Gemini response to OpenAI format
func (p *GeminiProvider) convertResponse(resp GeminiResponse, model string) *ChatResponse {
	// Extract text from first candidate
	var content string
	var finishReason openai.FinishReason = "stop"

	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]

		// Combine all text parts
		for _, part := range candidate.Content.Parts {
			content += part.Text
		}

		// Map finish reason
		finishReason = p.convertFinishReason(candidate.FinishReason)
	}

	// Create OpenAI-compatible response
	return &ChatResponse{
		ID:      fmt.Sprintf("gemini-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: content,
				},
				FinishReason: finishReason,
			},
		},
		Usage: openai.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		},
	}
}

// convertFinishReason converts Gemini's finish reason to OpenAI format
func (p *GeminiProvider) convertFinishReason(reason string) openai.FinishReason {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

// ChatCompletionStream makes a streaming chat completion request to Gemini
func (p *GeminiProvider) ChatCompletionStream(ctx context.Context, req ChatRequest) (*GeminiStreamReader, error) {
	// Convert OpenAI-style messages to Gemini format
	geminiReq := p.convertRequest(req)

	// Build streaming API URL
	// Use streamGenerateContent instead of generateContent
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?key=%s&alt=sse",
		req.Model, p.apiKey)

	// Marshal request
	reqBody, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Make request
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Gemini streaming API request failed: %w", err)
	}

	// Check for errors
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		body, _ := io.ReadAll(httpResp.Body)
		var geminiErr GeminiError
		if err := json.Unmarshal(body, &geminiErr); err == nil && geminiErr.ErrorDetails.Message != "" {
			return nil, fmt.Errorf("Gemini API error (status %d): %s", httpResp.StatusCode, geminiErr.ErrorDetails.Message)
		}
		return nil, fmt.Errorf("Gemini API error (status %d): %s", httpResp.StatusCode, string(body))
	}

	return &GeminiStreamReader{
		reader: bufio.NewReader(httpResp.Body),
		resp:   httpResp,
		model:  req.Model,
	}, nil
}

// Recv reads the next streaming chunk from Gemini
func (r *GeminiStreamReader) Recv() (openai.ChatCompletionStreamResponse, error) {
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

		// Gemini sends "data: <json>" format for SSE
		if strings.HasPrefix(line, "data: ") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

			// Parse the JSON chunk
			var geminiResp GeminiResponse
			if err := json.Unmarshal([]byte(dataStr), &geminiResp); err != nil {
				return openai.ChatCompletionStreamResponse{}, fmt.Errorf("failed to parse stream chunk: %w", err)
			}

			// Convert Gemini chunk to OpenAI format
			chunk := r.convertChunkToOpenAI(geminiResp)
			return chunk, nil
		}
	}
}

// Close closes the stream
func (r *GeminiStreamReader) Close() error {
	if r.resp != nil && r.resp.Body != nil {
		return r.resp.Body.Close()
	}
	return nil
}

// convertChunkToOpenAI converts Gemini streaming chunk to OpenAI format
func (r *GeminiStreamReader) convertChunkToOpenAI(resp GeminiResponse) openai.ChatCompletionStreamResponse {
	chunk := openai.ChatCompletionStreamResponse{
		ID:      fmt.Sprintf("gemini-stream-%d", time.Now().UnixNano()),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   r.model,
		Choices: []openai.ChatCompletionStreamChoice{},
	}

	// Check if we have candidates
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]

		// Extract text from parts
		var content string
		for _, part := range candidate.Content.Parts {
			content += part.Text
		}

		// Build choice
		choice := openai.ChatCompletionStreamChoice{
			Index: candidate.Index,
			Delta: openai.ChatCompletionStreamChoiceDelta{},
		}

		// If this is the first chunk, set role
		if candidate.Content.Role != "" {
			choice.Delta.Role = "assistant"
		}

		// If we have content, set it
		if content != "" {
			choice.Delta.Content = content
		}

		// If we have a finish reason, set it
		if candidate.FinishReason != "" {
			finishReason := convertGeminiFinishReason(candidate.FinishReason)
			choice.FinishReason = finishReason
		}

		chunk.Choices = []openai.ChatCompletionStreamChoice{choice}
	}

	// Add usage metadata if available
	if resp.UsageMetadata.TotalTokenCount > 0 {
		chunk.Usage = &openai.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	return chunk
}

// convertGeminiFinishReason converts Gemini's finish reason to OpenAI format
func convertGeminiFinishReason(reason string) openai.FinishReason {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

// ValidateModel claims Google's model namespace by prefix match.
//
// We intentionally skip a hard-coded allowlist: Google ships Gemini variants
// (2.0 / 2.5 / 3.x / flash / pro / lite / preview / dated pins) frequently,
// and users register pricing at runtime via scripts/manage_models.sh.
// A static list would force a rebuild for every new release.
//
// If the upstream API doesn't recognize the model, it returns a 404 which
// the failover executor treats as retriable.
func (p *GeminiProvider) ValidateModel(model string) bool {
	return strings.HasPrefix(model, "gemini-")
}
