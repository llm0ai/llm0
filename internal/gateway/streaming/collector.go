package streaming

import (
	"fmt"
	"time"

	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/sashabaranov/go-openai"
)

// StreamCollector collects chunks from a stream for post-processing
type StreamCollector struct {
	Chunks           []string
	FullContent      string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	StartTime        time.Time
	Model            string
	Provider         string
	FinishReason     string
}

// NewStreamCollector creates a new stream collector
func NewStreamCollector(provider, model string) *StreamCollector {
	return &StreamCollector{
		Chunks:    make([]string, 0),
		StartTime: time.Now(),
		Provider:  provider,
		Model:     model,
	}
}

// AddChunk adds a chunk to the collector
func (sc *StreamCollector) AddChunk(content string) {
	if content != "" {
		sc.Chunks = append(sc.Chunks, content)
		sc.FullContent += content
		// Note: We'll get actual token count from Usage at the end
		// This is just for intermediate tracking if needed
	}
}

// AddUsage updates token counts from final usage data
// This overrides any estimates with actual counts from the API
func (sc *StreamCollector) AddUsage(usage openai.Usage) {
	sc.PromptTokens = usage.PromptTokens
	sc.CompletionTokens = usage.CompletionTokens
	sc.TotalTokens = usage.TotalTokens
	fmt.Printf("📊 Usage info received: prompt=%d, completion=%d, total=%d\n",
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
}

// EstimateTokensIfNeeded estimates tokens if no usage data was provided
func (sc *StreamCollector) EstimateTokensIfNeeded() {
	// If we didn't get usage data from the API, estimate
	if sc.CompletionTokens == 0 && sc.FullContent != "" {
		sc.CompletionTokens = len(sc.FullContent) / 4
		fmt.Printf("⚠️ No usage data from API, estimated completion tokens: %d\n", sc.CompletionTokens)
	}
	if sc.TotalTokens == 0 {
		sc.TotalTokens = sc.PromptTokens + sc.CompletionTokens
	}
}

// SetFinishReason sets the finish reason
func (sc *StreamCollector) SetFinishReason(reason string) {
	sc.FinishReason = reason
}

// GetLatencyMs returns the total latency in milliseconds
func (sc *StreamCollector) GetLatencyMs() int {
	return int(time.Since(sc.StartTime).Milliseconds())
}

// ToResponse converts collected data to a ChatResponse
func (sc *StreamCollector) ToResponse() *providers.ChatResponse {
	return &providers.ChatResponse{
		ID:      "", // Will be set by caller
		Object:  "chat.completion",
		Created: sc.StartTime.Unix(),
		Model:   sc.Model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: sc.FullContent,
				},
				FinishReason: openai.FinishReason(sc.FinishReason),
			},
		},
		Usage: openai.Usage{
			PromptTokens:     sc.PromptTokens,
			CompletionTokens: sc.CompletionTokens,
			TotalTokens:      sc.TotalTokens,
		},
		LatencyMs: sc.GetLatencyMs(),
	}
}
