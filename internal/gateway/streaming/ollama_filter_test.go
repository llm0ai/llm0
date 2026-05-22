package streaming

import (
	"testing"

	"github.com/sashabaranov/go-openai"
)

func chunk(role, content string, finish openai.FinishReason) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Index:        0,
			FinishReason: finish,
			Delta: openai.ChatCompletionStreamChoiceDelta{
				Role:    role,
				Content: content,
			},
		}},
	}
}

func TestOllamaStreamFilter_NilForwardsEverything(t *testing.T) {
	var f *OllamaStreamFilter
	if !f.Forward(chunk("assistant", "", "")) {
		t.Fatal("nil filter must forward every chunk")
	}
}

func TestOllamaStreamFilter_DropsRepeatedEmptyRoleChunks(t *testing.T) {
	f := NewOllamaStreamFilter()
	roleOnly := chunk("assistant", "", "")

	if !f.Forward(roleOnly) {
		t.Fatal("first role chunk should be forwarded")
	}
	for i := 0; i < 5; i++ {
		if f.Forward(roleOnly) {
			t.Fatalf("duplicate empty role chunk %d should be dropped", i)
		}
	}
}

func TestOllamaStreamFilter_ForwardsContentAndFinish(t *testing.T) {
	f := NewOllamaStreamFilter()
	for i := 0; i < 3; i++ {
		_ = f.Forward(chunk("assistant", "", ""))
	}

	if !f.Forward(chunk("", "hello", "")) {
		t.Fatal("content chunk should be forwarded")
	}
	if !f.Forward(chunk("", "", openai.FinishReasonStop)) {
		t.Fatal("finish_reason chunk should be forwarded")
	}
}

func TestOllamaStreamFilter_ForwardsToolCalls(t *testing.T) {
	f := NewOllamaStreamFilter()
	ch := openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{Index: ptrInt(0)}},
			},
		}},
	}
	if !f.Forward(ch) {
		t.Fatal("chunk with tool_calls should be forwarded")
	}
}

func TestOllamaStreamFilter_ForwardsUsageOnlyChunks(t *testing.T) {
	f := NewOllamaStreamFilter()
	// OpenAI-style final usage frame: empty choices, usage populated.
	ch := openai.ChatCompletionStreamResponse{
		Choices: nil,
		Usage: &openai.Usage{
			PromptTokens:     5,
			CompletionTokens: 7,
			TotalTokens:      12,
		},
	}
	if !f.Forward(ch) {
		t.Fatal("usage-only chunk (no choices) must be forwarded")
	}
}

func TestOllamaStreamFilter_DropsFullyEmptyChunk(t *testing.T) {
	f := NewOllamaStreamFilter()
	empty := openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{},
		}},
	}
	if f.Forward(empty) {
		t.Fatal("fully empty delta with no finish_reason should be dropped")
	}
}

func ptrInt(v int) *int { return &v }
