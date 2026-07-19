package failover

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/sashabaranov/go-openai"
)

// fakeStream is a minimal StreamReader for tests. If firstErr is set, the
// first Recv() call returns it (simulating a stream that opened fine but
// died before yielding anything); otherwise it yields the queued chunks in
// order, then io.EOF.
type fakeStream struct {
	firstErr error
	chunks   []openai.ChatCompletionStreamResponse
	idx      int
	closed   bool
}

func (f *fakeStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if f.idx == 0 && f.firstErr != nil {
		f.idx++
		return openai.ChatCompletionStreamResponse{}, f.firstErr
	}
	if f.idx >= len(f.chunks) {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	c := f.chunks[f.idx]
	f.idx++
	return c, nil
}

func (f *fakeStream) Close() error {
	f.closed = true
	return nil
}

func chainOf(steps ...FailoverStep) *FailoverChain {
	return &FailoverChain{Steps: steps}
}

func step(provider string) FailoverStep {
	return FailoverStep{Provider: provider, Model: provider + "-model", ProviderName: provider}
}

func TestExecuteStream_FirstStepSucceeds(t *testing.T) {
	e := NewExecutor()
	fake := &fakeStream{chunks: []openai.ChatCompletionStreamResponse{{ID: "chunk-1"}}}
	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		return fake, nil
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "gpt-4o"}, chainOf(step("openai"), step("anthropic")), opener)

	if !result.Success {
		t.Fatalf("expected success, got error: %v", result.Error)
	}
	if result.FailoverOccurred {
		t.Error("expected no failover on first-step success")
	}
	if result.AttemptsCount != 1 {
		t.Errorf("expected 1 attempt, got %d", result.AttemptsCount)
	}
	if result.FirstChunk.ID != "chunk-1" {
		t.Errorf("expected first chunk to be returned, got %+v", result.FirstChunk)
	}
	if result.Stream == nil {
		t.Fatal("expected a live stream on success")
	}
}

func TestExecuteStream_FailoverToSecondStepOnOpenError(t *testing.T) {
	e := NewExecutor()
	winningStream := &fakeStream{chunks: []openai.ChatCompletionStreamResponse{{ID: "from-anthropic"}}}

	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		if s.Provider == "openai" {
			return nil, errors.New("502 bad gateway from provider")
		}
		return winningStream, nil
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "gpt-4o"}, chainOf(step("openai"), step("anthropic")), opener)

	if !result.Success {
		t.Fatalf("expected success after failover, got error: %v", result.Error)
	}
	if !result.FailoverOccurred {
		t.Error("expected FailoverOccurred=true")
	}
	if result.FinalProvider != "anthropic" {
		t.Errorf("expected final provider anthropic, got %s", result.FinalProvider)
	}
	if result.OriginalProvider != "openai" {
		t.Errorf("expected original provider openai, got %s", result.OriginalProvider)
	}
	if result.AttemptsCount != 2 {
		t.Errorf("expected 2 attempts, got %d", result.AttemptsCount)
	}
}

func TestExecuteStream_FirstRecvFailure_ClosesStreamAndTriesNext(t *testing.T) {
	e := NewExecutor()
	deadStream := &fakeStream{firstErr: errors.New("connection reset by peer")}
	winningStream := &fakeStream{chunks: []openai.ChatCompletionStreamResponse{{ID: "ok"}}}

	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		if s.Provider == "openai" {
			return deadStream, nil
		}
		return winningStream, nil
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "gpt-4o"}, chainOf(step("openai"), step("anthropic")), opener)

	if !result.Success {
		t.Fatalf("expected success after failover, got error: %v", result.Error)
	}
	if !deadStream.closed {
		t.Error("expected the dead stream to be closed after its first Recv() failed")
	}
	if result.FinalProvider != "anthropic" {
		t.Errorf("expected final provider anthropic, got %s", result.FinalProvider)
	}
}

func TestExecuteStream_NonRetriableErrorStopsChain(t *testing.T) {
	e := NewExecutor()
	secondStepCalled := false

	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		if s.Provider == "openai" {
			return nil, errors.New("status 400: malformed request body, missing required field 'model'")
		}
		secondStepCalled = true
		return &fakeStream{chunks: []openai.ChatCompletionStreamResponse{{}}}, nil
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "gpt-4o"}, chainOf(step("openai"), step("anthropic")), opener)

	if result.Success {
		t.Fatal("expected failure for a non-retriable error")
	}
	if secondStepCalled {
		t.Error("non-retriable error should stop the chain, not try the next step")
	}
	if result.AttemptsCount != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", result.AttemptsCount)
	}
}

func TestExecuteStream_AllStepsFail(t *testing.T) {
	e := NewExecutor()
	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		return nil, errors.New("503 service unavailable")
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "gpt-4o"}, chainOf(step("openai"), step("anthropic"), step("google")), opener)

	if result.Success {
		t.Fatal("expected failure when every step fails")
	}
	if result.Error == nil {
		t.Fatal("expected a non-nil error")
	}
	if result.AttemptsCount != 3 {
		t.Errorf("expected 3 attempts, got %d", result.AttemptsCount)
	}
	if result.FinalProvider != "google" {
		t.Errorf("expected final provider to be the last attempted (google), got %s", result.FinalProvider)
	}
}

func TestExecuteStream_NilChain_FallsBackToModelDetection(t *testing.T) {
	e := NewExecutor()
	var openedProvider string
	opener := func(ctx context.Context, s FailoverStep, req providers.ChatRequest) (StreamReader, error) {
		openedProvider = s.Provider
		return &fakeStream{chunks: []openai.ChatCompletionStreamResponse{{}}}, nil
	}

	result := e.ExecuteStream(context.Background(), providers.ChatRequest{Model: "claude-sonnet-4-6"}, nil, opener)

	if !result.Success {
		t.Fatalf("expected success, got error: %v", result.Error)
	}
	if openedProvider != "anthropic" {
		t.Errorf("expected model detection to route claude-* to anthropic, got %s", openedProvider)
	}
}
