package failover

import (
	"testing"

	"github.com/llm0ai/llm0/internal/shared/config"
)

// cfgWithOllama returns a config with Ollama enabled and model tier mappings set.
func cfgWithOllama(mode string) *config.Config {
	return &config.Config{
		FailoverMode:        mode,
		OllamaBaseURL:       "http://localhost:11434/v1",
		OllamaModelFlagship: "llama3.3:70b",
		OllamaModelBalanced: "qwen2.5:14b",
		OllamaModelBudget:   "gemma3:4b",
	}
}

func cfgNoOllama(mode string) *config.Config {
	return &config.Config{
		FailoverMode:  mode,
		OllamaBaseURL: "",
	}
}

// ── cloud_only ────────────────────────────────────────────────────────────────

func TestGetFailoverChain_CloudOnly_KnownModel(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgNoOllama("cloud_only"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	for _, step := range chain.Steps {
		if step.Provider == "ollama" {
			t.Errorf("cloud_only chain should not contain Ollama; steps: %v", chain.Steps)
		}
	}
	if chain.Steps[0].Provider != "openai" {
		t.Errorf("first step should be openai, got %s", chain.Steps[0].Provider)
	}
}

func TestGetFailoverChain_CloudOnly_UnknownModel_ReturnsNil(t *testing.T) {
	chain := GetFailoverChain("unknown-model-xyz", cfgNoOllama("cloud_only"))
	if chain != nil {
		t.Errorf("expected nil for unknown model in cloud_only mode, got %v", chain)
	}
}

// ── cloud_first ───────────────────────────────────────────────────────────────

func TestGetFailoverChain_CloudFirst_OllamaAppended(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgWithOllama("cloud_first"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	last := chain.Steps[len(chain.Steps)-1]
	if last.Provider != "ollama" {
		t.Errorf("cloud_first: expected Ollama as last step, got %s", last.Provider)
	}
	if chain.Steps[0].Provider == "ollama" {
		t.Error("cloud_first: Ollama should NOT be first step")
	}
}

func TestGetFailoverChain_CloudFirst_FlagshipTierOllama(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgWithOllama("cloud_first"))
	last := chain.Steps[len(chain.Steps)-1]
	// gpt-4o is "flagship" tier → should map to OllamaModelFlagship
	if last.Model != "llama3.3:70b" {
		t.Errorf("expected flagship ollama model 'llama3.3:70b', got %s", last.Model)
	}
}

func TestGetFailoverChain_CloudFirst_BudgetTierOllama(t *testing.T) {
	chain := GetFailoverChain("gpt-3.5-turbo", cfgWithOllama("cloud_first"))
	last := chain.Steps[len(chain.Steps)-1]
	// gpt-3.5-turbo is "budget" tier → should map to OllamaModelBudget
	if last.Model != "gemma3:4b" {
		t.Errorf("expected budget ollama model 'gemma3:4b', got %s", last.Model)
	}
}

// ── local_first ───────────────────────────────────────────────────────────────

func TestGetFailoverChain_LocalFirst_OllamaPrepended(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgWithOllama("local_first"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	if chain.Steps[0].Provider != "ollama" {
		t.Errorf("local_first: expected Ollama as first step, got %s", chain.Steps[0].Provider)
	}
}

func TestGetFailoverChain_LocalFirst_CloudStepsFollow(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgWithOllama("local_first"))
	// Steps after Ollama should all be cloud providers.
	for _, step := range chain.Steps[1:] {
		if step.Provider == "ollama" {
			t.Errorf("local_first: Ollama appears more than once: %v", chain.Steps)
		}
	}
}

// ── local_only ────────────────────────────────────────────────────────────────

func TestGetFailoverChain_LocalOnly_SingleOllamaStep(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgWithOllama("local_only"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	if len(chain.Steps) != 1 {
		t.Errorf("local_only: expected exactly 1 step, got %d", len(chain.Steps))
	}
	if chain.Steps[0].Provider != "ollama" {
		t.Errorf("local_only: step should be ollama, got %s", chain.Steps[0].Provider)
	}
}

func TestGetFailoverChain_LocalOnly_NoOllama_ReturnsNil(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", cfgNoOllama("local_only"))
	if chain != nil {
		t.Errorf("local_only with no Ollama configured should return nil, got %v", chain)
	}
}

// ── nil / fallback behaviour ──────────────────────────────────────────────────

func TestGetFailoverChain_NilConfig_DefaultsToCloudFirst(t *testing.T) {
	chain := GetFailoverChain("gpt-4o", nil)
	if chain == nil {
		t.Fatal("expected chain with nil config, got nil")
	}
	// Nil config means no Ollama → should be a pure cloud chain.
	for _, step := range chain.Steps {
		if step.Provider == "ollama" {
			t.Errorf("nil config should produce no Ollama steps; got %v", chain.Steps)
		}
	}
}

func TestGetFailoverChain_UnknownModel_WithOllama_ReturnsSingleOllamaStep(t *testing.T) {
	// Unknown model + Ollama available → should still serve via Ollama.
	chain := GetFailoverChain("mystery-model", cfgWithOllama("cloud_first"))
	if chain == nil {
		t.Fatal("expected fallback Ollama chain for unknown model")
	}
	if len(chain.Steps) != 1 || chain.Steps[0].Provider != "ollama" {
		t.Errorf("unexpected chain for unknown model: %v", chain.Steps)
	}
}

// ── HasFailoverChain ──────────────────────────────────────────────────────────

func TestHasFailoverChain_KnownModel(t *testing.T) {
	if !HasFailoverChain("gpt-4o", cfgNoOllama("cloud_only")) {
		t.Error("gpt-4o should have a failover chain")
	}
}

func TestHasFailoverChain_UnknownModel_NoOllama(t *testing.T) {
	if HasFailoverChain("mystery-model", cfgNoOllama("cloud_only")) {
		t.Error("unknown model with no Ollama should not have a chain")
	}
}

// ── Config-driven cross-provider derivation ────────────────────────────────
//
// These replace the old hand-maintained DefaultFailoverChains map: the
// chain is derived from FAILOVER_<PROVIDER>_FLAGSHIP/_CHEAP instead of a
// per-model entry, so a brand-new model name (never seen before) still
// gets a correct chain as long as its name follows the usual conventions.

func TestGetFailoverChain_Derived_FlagshipClassUsesFlagshipFallbacks(t *testing.T) {
	// "gpt-4o" has no cheap-tier keyword → flagship class → every other
	// provider's *flagship* model, not its cheap one.
	chain := GetFailoverChain("gpt-4o", cfgNoOllama("cloud_only"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	want := []FailoverStep{
		{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
		{Provider: "anthropic", Model: config.DefaultFailoverAnthropicFlagship, ProviderName: "Anthropic"},
		{Provider: "google", Model: config.DefaultFailoverGoogleFlagship, ProviderName: "Google"},
	}
	assertSteps(t, chain.Steps, want)
}

func TestGetFailoverChain_Derived_CheapClassUsesCheapFallbacks(t *testing.T) {
	// "gpt-4o-mini" → budget/cheap class → every other provider's cheap model.
	chain := GetFailoverChain("gpt-4o-mini", cfgNoOllama("cloud_only"))
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	want := []FailoverStep{
		{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
		{Provider: "anthropic", Model: config.DefaultFailoverAnthropicCheap, ProviderName: "Anthropic"},
		{Provider: "google", Model: config.DefaultFailoverGoogleCheap, ProviderName: "Google"},
	}
	assertSteps(t, chain.Steps, want)
}

func TestGetFailoverChain_Derived_UnseenModelName_StillGetsChain(t *testing.T) {
	// A model that has never been hardcoded anywhere still resolves,
	// because classification is name-heuristic-based, not a lookup table.
	chain := GetFailoverChain("gpt-9.9-turbo-mini", cfgNoOllama("cloud_only"))
	if chain == nil {
		t.Fatal("expected chain for a never-seen-before OpenAI model name")
	}
	if chain.Steps[0].Model != "gpt-9.9-turbo-mini" {
		t.Errorf("expected origin step to keep the exact requested model, got %s", chain.Steps[0].Model)
	}
	if len(chain.Steps) != 3 {
		t.Fatalf("expected 3 steps (origin + 2 other providers), got %d: %v", len(chain.Steps), chain.Steps)
	}
	// "mini" → cheap class.
	if chain.Steps[1].Model != config.DefaultFailoverAnthropicCheap {
		t.Errorf("expected anthropic cheap fallback, got %s", chain.Steps[1].Model)
	}
}

func TestGetFailoverChain_Derived_RespectsProviderOrder(t *testing.T) {
	cfg := &config.Config{
		FailoverMode:          "cloud_only",
		FailoverProviderOrder: "google,anthropic,openai",
	}
	chain := GetFailoverChain("gpt-4o", cfg)
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	// Origin step is always first regardless of order; the order only
	// governs the OTHER providers appended after it.
	if chain.Steps[0].Provider != "openai" {
		t.Fatalf("origin step should always be first, got %s", chain.Steps[0].Provider)
	}
	if chain.Steps[1].Provider != "google" || chain.Steps[2].Provider != "anthropic" {
		t.Errorf("expected [openai, google, anthropic], got %v", chain.Steps)
	}
}

func TestGetFailoverChain_Derived_EnvOverrideChangesFallbackModel(t *testing.T) {
	cfg := &config.Config{
		FailoverMode:              "cloud_only",
		FailoverAnthropicFlagship: "claude-custom-flagship",
	}
	chain := GetFailoverChain("gpt-4o", cfg)
	if chain == nil {
		t.Fatal("expected chain, got nil")
	}
	for _, step := range chain.Steps {
		if step.Provider == "anthropic" && step.Model != "claude-custom-flagship" {
			t.Errorf("expected overridden anthropic model, got %s", step.Model)
		}
	}
}

func TestKnownCloudModels_DefaultsToSixModels(t *testing.T) {
	models := KnownCloudModels(nil)
	if len(models) != 6 {
		t.Fatalf("expected 6 known models (flagship+cheap x 3 providers), got %d: %v", len(models), models)
	}
}

func TestKnownCloudModels_Dedupes(t *testing.T) {
	// If a deployment (mis)configures the same model for both classes on
	// one provider, KnownCloudModels should list it once, not twice.
	cfg := &config.Config{
		FailoverOpenAIFlagship: "gpt-same",
		FailoverOpenAICheap:    "gpt-same",
	}
	models := KnownCloudModels(cfg)
	count := 0
	for _, m := range models {
		if m.Provider == "openai" && m.Model == "gpt-same" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 deduped entry for openai/gpt-same, got %d", count)
	}
}

func assertSteps(t *testing.T, got []FailoverStep, want []FailoverStep) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %d steps, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: expected %+v, got %+v", i, want[i], got[i])
		}
	}
}
