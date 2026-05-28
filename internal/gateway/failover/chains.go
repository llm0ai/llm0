package failover

import (
	"strings"

	"github.com/llm0ai/llm0/internal/shared/config"
)

// Preset failover chains for Pro tier users
// Based on best cost/performance ratios and reliability
//
// Strategy:
// - Primary: Best cost/performance for the model class
// - First fallback: Different provider, similar capability
// - Second fallback: Budget option, still good quality
//
// Models verified as of Feb 2026:
// - OpenAI: gpt-4o, gpt-4o-mini, gpt-4-turbo, gpt-3.5-turbo
// - Anthropic: claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5-20251001,
//              claude-sonnet-4-5-20250929, claude-opus-4-5-20251101, claude-3-haiku-20240307
// - Google: gemini-2.5-flash, gemini-2.5-pro, gemini-2.0-flash

// DefaultFailoverChains defines preset failover chains for different model classes
var DefaultFailoverChains = map[string]FailoverChain{
	// ── OpenAI flagship ──────────────────────────────────────────────────────────
	"gpt-4o": {
		Steps: []FailoverStep{
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderName: "Anthropic"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},
	"gpt-4-turbo": {
		Steps: []FailoverStep{
			{Provider: "openai", Model: "gpt-4-turbo", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderName: "Anthropic"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},

	// ── OpenAI cost-optimized ─────────────────────────────────────────────────
	"gpt-4o-mini": {
		Steps: []FailoverStep{
			{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", ProviderName: "Anthropic"},
			{Provider: "google", Model: "gemini-2.5-flash", ProviderName: "Google"},
		},
	},
	"gpt-3.5-turbo": {
		Steps: []FailoverStep{
			{Provider: "openai", Model: "gpt-3.5-turbo", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-3-haiku-20240307", ProviderName: "Anthropic"},
			{Provider: "google", Model: "gemini-2.0-flash", ProviderName: "Google"},
		},
	},

	// ── Anthropic Claude 4.6 (latest) ────────────────────────────────────────
	"claude-sonnet-4-6": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},
	"claude-opus-4-6": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-opus-4-6", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},

	// ── Anthropic Claude 4.5 ─────────────────────────────────────────────────
	"claude-opus-4-5-20251101": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-opus-4-5-20251101", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},
	"claude-sonnet-4-5-20250929": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-sonnet-4-5-20250929", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},
	"claude-haiku-4-5-20251001": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-flash", ProviderName: "Google"},
		},
	},

	// ── Anthropic Claude 4 ───────────────────────────────────────────────────
	"claude-sonnet-4-20250514": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-sonnet-4-20250514", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},
	"claude-opus-4-20250514": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-opus-4-20250514", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
		},
	},

	// ── Anthropic Claude 3 ───────────────────────────────────────────────────
	"claude-3-haiku-20240307": {
		Steps: []FailoverStep{
			{Provider: "anthropic", Model: "claude-3-haiku-20240307", ProviderName: "Anthropic"},
			{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
			{Provider: "google", Model: "gemini-2.0-flash", ProviderName: "Google"},
		},
	},

	// ── Google Gemini ─────────────────────────────────────────────────────────
	"gemini-2.5-pro": {
		Steps: []FailoverStep{
			{Provider: "google", Model: "gemini-2.5-pro", ProviderName: "Google"},
			{Provider: "openai", Model: "gpt-4o", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderName: "Anthropic"},
		},
	},
	"gemini-2.5-flash": {
		Steps: []FailoverStep{
			{Provider: "google", Model: "gemini-2.5-flash", ProviderName: "Google"},
			{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", ProviderName: "Anthropic"},
		},
	},
	"gemini-2.0-flash": {
		Steps: []FailoverStep{
			{Provider: "google", Model: "gemini-2.0-flash", ProviderName: "Google"},
			{Provider: "openai", Model: "gpt-4o-mini", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-3-haiku-20240307", ProviderName: "Anthropic"},
		},
	},
	"gemini-2.0-flash-lite": {
		Steps: []FailoverStep{
			{Provider: "google", Model: "gemini-2.0-flash-lite", ProviderName: "Google"},
			{Provider: "openai", Model: "gpt-3.5-turbo", ProviderName: "OpenAI"},
			{Provider: "anthropic", Model: "claude-3-haiku-20240307", ProviderName: "Anthropic"},
		},
	},
}

// ModelTierMap classifies each cloud model into a quality tier used to select
// the appropriate Ollama model when Ollama is in the failover chain.
//
//   flagship — gpt-4o / claude-opus / gemini-pro class
//   balanced — gpt-4o-mini / claude-sonnet / gemini-flash class
//   budget   — gpt-3.5 / claude-haiku / gemini-flash-lite class
var ModelTierMap = map[string]string{
	// OpenAI
	"gpt-4o":            "flagship",
	"gpt-4-turbo":       "flagship",
	"gpt-4o-mini":       "balanced",
	"gpt-3.5-turbo":     "budget",
	"gpt-3.5-turbo-16k": "budget",
	// Anthropic
	"claude-opus-4-6":           "flagship",
	"claude-opus-4-5-20251101":  "flagship",
	"claude-opus-4-20250514":    "flagship",
	"claude-sonnet-4-6":         "balanced",
	"claude-sonnet-4-5-20250929": "balanced",
	"claude-sonnet-4-20250514":  "balanced",
	"claude-haiku-4-5-20251001": "budget",
	"claude-3-haiku-20240307":   "budget",
	// Google
	"gemini-2.5-pro":      "flagship",
	"gemini-2.5-flash":    "balanced",
	"gemini-2.0-flash":    "budget",
	"gemini-2.0-flash-lite": "budget",
}

// ollamaStepForModel returns the Ollama failover step appropriate for the given
// cloud model's quality tier, or an empty FailoverStep when Ollama is not configured.
func ollamaStepForModel(model string, cfg *config.Config) *FailoverStep {
	if cfg == nil || cfg.OllamaBaseURL == "" {
		return nil
	}

	tier := ModelTierMap[model]
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

	// Resolve the base cloud chain (exact match, then prefix match).
	var cloudChain *FailoverChain
	if chain, ok := DefaultFailoverChains[model]; ok {
		c := chain // copy
		cloudChain = &c
	} else {
		for baseModel, chain := range DefaultFailoverChains {
			if strings.HasPrefix(model, baseModel) {
				c := chain
				cloudChain = &c
				break
			}
		}
	}

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
