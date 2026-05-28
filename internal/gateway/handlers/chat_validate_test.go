package handlers

import (
	"testing"

	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/sashabaranov/go-openai"
)

func TestValidateChatRequest_EmptyMessages(t *testing.T) {
	req := providers.ChatRequest{Model: "gemma3:4b", Stream: true}
	if got := validateChatRequest(req); got == "" {
		t.Fatal("expected error for empty messages")
	}
}

func TestValidateChatRequest_EmptyModel(t *testing.T) {
	req := providers.ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	if got := validateChatRequest(req); got == "" {
		t.Fatal("expected error for empty model")
	}
}

func TestValidateChatRequest_MissingRole(t *testing.T) {
	req := providers.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessage{
			{Content: "hi"},
		},
	}
	if got := validateChatRequest(req); got == "" {
		t.Fatal("expected error for missing role")
	}
}

func TestValidateChatRequest_OK(t *testing.T) {
	req := providers.ChatRequest{
		Model: "gemma3:4b",
		Messages: []openai.ChatCompletionMessage{
			{Role: "user", Content: "hi"},
		},
	}
	if got := validateChatRequest(req); got != "" {
		t.Fatalf("unexpected error: %q", got)
	}
}
