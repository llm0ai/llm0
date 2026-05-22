package streaming

import (
	"github.com/sashabaranov/go-openai"
)

// OllamaStreamFilter decides whether each SSE chunk from Ollama's
// OpenAI-compatible adapter is worth forwarding to the client. Ollama can emit
// long runs of {"delta":{"role":"assistant"}} frames before the first content
// token; this filter keeps the first role chunk, every chunk that carries
// content / tool_calls / finish_reason / usage, and drops the rest.
//
// The filter affects the *client write* only — the gateway's internal
// StreamCollector (cost, caching, logs) always sees every chunk.
type OllamaStreamFilter struct {
	roleChunkForwarded bool
}

// NewOllamaStreamFilter creates a filter for one streaming request. A nil
// filter means "no filtering" — callers should construct one only when the
// provider is Ollama and OLLAMA_FILTER_EMPTY_CHUNKS is true.
func NewOllamaStreamFilter() *OllamaStreamFilter {
	return &OllamaStreamFilter{}
}

// Forward reports whether the chunk should be written to the client.
// A nil filter forwards everything.
func (f *OllamaStreamFilter) Forward(chunk openai.ChatCompletionStreamResponse) bool {
	if f == nil {
		return true
	}

	// Usage-only / metadata chunks (no choices) always pass through.
	if len(chunk.Choices) == 0 {
		return true
	}

	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		return true
	}

	delta := choice.Delta
	if delta.Content != "" || len(delta.ToolCalls) > 0 {
		return true
	}

	// First role assignment (e.g. delta.role=assistant) — keep once.
	if delta.Role != "" {
		if !f.roleChunkForwarded {
			f.roleChunkForwarded = true
			return true
		}
		return false
	}

	return false
}
