package handlers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm0ai/llm0/internal/gateway/failover"
)

// modelObject mirrors the OpenAI /v1/models response shape.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ListModels handles GET /v1/models
//
// Returns every model that the gateway can route, including:
//   - All cloud models with a known failover chain
//   - Ollama models (fetched live from the local instance) when OLLAMA_BASE_URL is set
//
// The response is OpenAI-compatible so existing OpenAI SDKs work without changes.
func (h *ChatHandler) ListModels(c *gin.Context) {
	var models []modelObject

	mode := h.cfg.FailoverMode

	// ── Cloud models ──────────────────────────────────────────────────────────
	// KnownCloudModels is the curated, config-driven default set (not
	// exhaustive — any gpt-*/claude-*/gemini-* model works for chat
	// completions; see failover/chains.go).
	if mode != "local_only" {
		for _, step := range failover.KnownCloudModels(h.cfg) {
			// Only list models that are reachable given the configured API keys.
			if !h.hasReachableProvider(failover.FailoverChain{Steps: []failover.FailoverStep{step}}) {
				continue
			}
			models = append(models, modelObject{
				ID:      step.Model,
				Object:  "model",
				Created: 1700000000,
				OwnedBy: cloudOwner(step.Provider),
			})
		}
	}

	// ── Ollama models (live) ──────────────────────────────────────────────────
	if mode != "cloud_only" && h.ollamaProvider != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		ollamaModels, err := h.ollamaProvider.ListModels(ctx)
		if err != nil {
			// Non-fatal: Ollama might be down; still return cloud list.
			fmt.Printf("⚠️  Could not list Ollama models: %v\n", err)
		} else {
			for _, id := range ollamaModels {
				models = append(models, modelObject{
					ID:      id,
					Object:  "model",
					Created: time.Now().Unix(),
					OwnedBy: "ollama",
				})
			}
		}
	}

	// Stable alphabetical order for predictable clients.
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})

	c.JSON(200, gin.H{
		"object": "list",
		"data":   models,
	})
}

// hasReachableProvider returns true when at least one step in the chain has
// a configured API key (i.e., the gateway can actually call that provider).
func (h *ChatHandler) hasReachableProvider(chain failover.FailoverChain) bool {
	for _, step := range chain.Steps {
		switch step.Provider {
		case "openai":
			if h.cfg.OpenAIAPIKey != "" {
				return true
			}
		case "anthropic":
			if h.cfg.AnthropicAPIKey != "" {
				return true
			}
		case "google":
			if h.cfg.GeminiAPIKey != "" {
				return true
			}
		}
	}
	return false
}

// cloudOwner maps internal provider names to OpenAI-compatible owned_by strings.
func cloudOwner(provider string) string {
	switch provider {
	case "openai":
		return "openai"
	case "anthropic":
		return "anthropic"
	case "google":
		return "google"
	default:
		return provider
	}
}
