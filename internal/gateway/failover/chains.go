package failover

import (
	"strings"

	"github.com/llm0ai/llm0/internal/shared/config"
)

// Cross-provider failover, config-driven.
//
// Historically this file hand-enumerated one FailoverChain per known model
// (~15 entries) plus a per-model tier map. That drifts the moment a new
// model ships — it already had, silently: schema/seed_models.sql lists
// gpt-5.4 and claude-opus-4-7, neither of which had a chain here.
//
// The fix: classify the *requested* model into a cost class ("flagship" or
// "cheap") from its name, then ask each other provider "what's your model
// for that class?" — a question answered by six config values
// (FAILOVER_<PROVIDER>_FLAGSHIP / _CHEAP, see internal/shared/config) instead
// of by a hand-maintained map. A new model release means bumping a config
// value (or the code default), never adding a map entry.
//
// Per-project overrides (managed platform) layer on top of this same
// resolver later — they'll set cfg-equivalent values per project instead of
// per deployment, without changing this algorithm.

// classifyTier buckets a model name into a rough quality/cost tier from
// naming conventions alone. This never needs updating for new models: model
// families keep using "mini"/"haiku"/"flash"/etc. for their cheap tier.
// Used for two things: (1) collapsed to flagship/cheap for cross-provider
// failover target selection, and (2) as-is (three buckets) for sizing the
// local Ollama model when Ollama is in the chain.
func classifyTier(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "nano"), strings.Contains(m, "mini"),
		strings.Contains(m, "haiku"), strings.Contains(m, "lite"),
		strings.Contains(m, "3.5"):
		return "budget"
	case strings.Contains(m, "flash"), strings.Contains(m, "sonnet"):
		return "balanced"
	default:
		// Includes "opus", "pro", "gpt-4o", "gpt-5.x", "turbo", and any
		// model name we don't recognize — defaulting unknown models to
		// flagship (rather than the cheapest local model) is the safer
		// failure mode for quality.
		return "flagship"
	}
}

// cloudClass collapses the three-bucket tier down to the two classes
// FAILOVER_<PROVIDER>_* config exposes. "balanced" and "budget" both fail
// over to the other providers' cheap model — the only thing that matters
// for cross-provider failover is "don't jump a cheap request to a flagship
// price tag," not fine-grained tier parity.
func cloudClass(tier string) string {
	if tier == "flagship" {
		return "flagship"
	}
	return "cheap"
}

// providerDisplayName returns the human-readable name used in logs/UI.
func providerDisplayName(provider string) string {
	switch provider {
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "google":
		return "Google"
	case "ollama":
		return "Ollama"
	default:
		return provider
	}
}

// providerOrder parses FailoverProviderOrder ("openai,anthropic,google")
// into a normalized slice, falling back to the code default when cfg is nil
// or the field is unset (e.g. a Config built directly in tests).
func providerOrder(cfg *config.Config) []string {
	raw := config.DefaultFailoverProviderOrder
	if cfg != nil && cfg.FailoverProviderOrder != "" {
		raw = cfg.FailoverProviderOrder
	}

	parts := strings.Split(raw, ",")
	order := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			order = append(order, p)
		}
	}
	return order
}

// providerDefaultModel returns the configured model for the given provider
// + cost class ("flagship" | "cheap"), falling back to the code default in
// internal/shared/config when cfg is nil or the specific field is unset.
// Returns "" for an unrecognized provider (e.g. "ollama", handled
// separately by ollamaStepForModel).
func providerDefaultModel(provider, class string, cfg *config.Config) string {
	var flagship, cheap string
	if cfg != nil {
		switch provider {
		case "openai":
			flagship, cheap = cfg.FailoverOpenAIFlagship, cfg.FailoverOpenAICheap
		case "anthropic":
			flagship, cheap = cfg.FailoverAnthropicFlagship, cfg.FailoverAnthropicCheap
		case "google":
			flagship, cheap = cfg.FailoverGoogleFlagship, cfg.FailoverGoogleCheap
		}
	}

	switch provider {
	case "openai":
		if flagship == "" {
			flagship = config.DefaultFailoverOpenAIFlagship
		}
		if cheap == "" {
			cheap = config.DefaultFailoverOpenAICheap
		}
	case "anthropic":
		if flagship == "" {
			flagship = config.DefaultFailoverAnthropicFlagship
		}
		if cheap == "" {
			cheap = config.DefaultFailoverAnthropicCheap
		}
	case "google":
		if flagship == "" {
			flagship = config.DefaultFailoverGoogleFlagship
		}
		if cheap == "" {
			cheap = config.DefaultFailoverGoogleCheap
		}
	default:
		return ""
	}

	if class == "flagship" {
		return flagship
	}
	return cheap
}

// buildCloudChain derives the cross-provider chain for a request: the
// requested model itself, then each other configured provider's default
// model for the same cost class, in providerOrder. Returns nil when the
// model's origin provider can't be determined (unknown model name) — the
// caller then falls back to Ollama-only or nil, same as before.
func buildCloudChain(model string, cfg *config.Config) *FailoverChain {
	originProvider := detectProviderForModel(model)
	if originProvider == "" {
		return nil
	}

	class := cloudClass(classifyTier(model))

	steps := []FailoverStep{
		{Provider: originProvider, Model: model, ProviderName: providerDisplayName(originProvider)},
	}

	for _, p := range providerOrder(cfg) {
		if p == originProvider {
			continue
		}
		fallbackModel := providerDefaultModel(p, class, cfg)
		if fallbackModel == "" {
			continue
		}
		steps = append(steps, FailoverStep{
			Provider:     p,
			Model:        fallbackModel,
			ProviderName: providerDisplayName(p),
		})
	}

	return &FailoverChain{Steps: steps}
}

// KnownCloudModels returns the curated set of cross-provider default models
// (flagship + cheap per configured provider) — used by GET /v1/models to
// advertise a model list without hand-maintaining one. This is deliberately
// not exhaustive (any gpt-*/claude-*/gemini-* model works for chat
// completions via buildCloudChain above); it's the small "known-good" set
// this deployment's config points at.
func KnownCloudModels(cfg *config.Config) []FailoverStep {
	var out []FailoverStep
	seen := make(map[string]bool)

	for _, p := range providerOrder(cfg) {
		for _, class := range [...]string{"flagship", "cheap"} {
			model := providerDefaultModel(p, class, cfg)
			if model == "" {
				continue
			}
			key := p + ":" + model
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, FailoverStep{Provider: p, Model: model, ProviderName: providerDisplayName(p)})
		}
	}
	return out
}

// ollamaStepForModel returns the Ollama failover step appropriate for the given
// cloud model's quality tier, or an empty FailoverStep when Ollama is not configured.
func ollamaStepForModel(model string, cfg *config.Config) *FailoverStep {
	if cfg == nil || cfg.OllamaBaseURL == "" {
		return nil
	}

	tier := classifyTier(model)
	var localModel string
	switch tier {
	case "flagship":
		localModel = cfg.OllamaModelFlagship
	case "balanced":
		localModel = cfg.OllamaModelBalanced
	default:
		localModel = cfg.OllamaModelBudget
	}
	if localModel == "" {
		return nil
	}
	return &FailoverStep{Provider: "ollama", Model: localModel, ProviderName: "Ollama"}
}

// GetFailoverChain returns the failover chain for a given model, adjusted for
// the configured FAILOVER_MODE.
//
// Modes:
//
//	cloud_first  — cloud providers first, Ollama appended as last-resort fallback (default)
//	local_first  — Ollama prepended as first attempt, cloud providers follow
//	local_only   — single-step chain pointing at the appropriate Ollama model
//	cloud_only   — standard cloud-only chain, Ollama never used
//
// Returns nil when no chain can be built (unknown model + no Ollama configured).
func GetFailoverChain(model string, cfg *config.Config) *FailoverChain {
	mode := "cloud_first"
	if cfg != nil && cfg.FailoverMode != "" {
		mode = cfg.FailoverMode
	}

	// local_only: bypass cloud chain entirely.
	if mode == "local_only" {
		step := ollamaStepForModel(model, cfg)
		if step == nil {
			return nil
		}
		return &FailoverChain{Steps: []FailoverStep{*step}}
	}

	// Resolve the base cloud chain (derived from provider defaults, not a map).
	cloudChain := buildCloudChain(model, cfg)

	// cloud_only: return cloud chain as-is (or nil if unknown model).
	if mode == "cloud_only" {
		return cloudChain
	}

	ollamaStep := ollamaStepForModel(model, cfg)

	// No Ollama configured — behave like cloud_only regardless of mode.
	if ollamaStep == nil {
		return cloudChain
	}

	// If model is completely unknown to cloud providers and we have Ollama,
	// build a single-step Ollama chain so the request still goes somewhere.
	if cloudChain == nil {
		return &FailoverChain{Steps: []FailoverStep{*ollamaStep}}
	}

	switch mode {
	case "local_first":
		// Prepend Ollama before the cloud steps.
		steps := make([]FailoverStep, 0, len(cloudChain.Steps)+1)
		steps = append(steps, *ollamaStep)
		steps = append(steps, cloudChain.Steps...)
		return &FailoverChain{Steps: steps}

	default: // cloud_first
		// Append Ollama after the cloud steps.
		steps := make([]FailoverStep, 0, len(cloudChain.Steps)+1)
		steps = append(steps, cloudChain.Steps...)
		steps = append(steps, *ollamaStep)
		return &FailoverChain{Steps: steps}
	}
}

// HasFailoverChain checks if a model has a defined failover chain.
func HasFailoverChain(model string, cfg *config.Config) bool {
	return GetFailoverChain(model, cfg) != nil
}
