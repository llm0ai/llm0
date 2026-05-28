package cost

import (
	"context"
	"fmt"
	"strings"

	"github.com/llm0ai/llm0/internal/shared/database"
)

// Calculator handles cost calculation for LLM requests
type Calculator struct {
	db      *database.DB
	pricing map[string]ModelPricing // Cache for pricing data
}

// ModelPricing represents pricing for a specific model
type ModelPricing struct {
	Provider          string
	Model             string
	InputPer1MTokens  float64 // Cost per 1M input tokens in USD
	OutputPer1MTokens float64 // Cost per 1M output tokens in USD
}

// NewCalculator creates a new cost calculator with current pricing
func NewCalculator(db *database.DB) *Calculator {
	calc := &Calculator{
		db:      db,
		pricing: defaultPricing(), // Fallback pricing
	}
	// Load pricing from database
	calc.loadPricingFromDB()
	return calc
}

// loadPricingFromDB loads pricing from the model_pricing table
func (c *Calculator) loadPricingFromDB() {
	if c.db == nil {
		fmt.Println("⚠️ No database connection, using fallback pricing")
		return
	}

	ctx := context.Background()
	query := `
		SELECT provider, model, 
		       input_per_1k_tokens * 1000 as input_per_1m, 
		       output_per_1k_tokens * 1000 as output_per_1m
		FROM model_pricing
	`
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		fmt.Printf("⚠️ Failed to load pricing from DB: %v (using fallback)\n", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var provider, model string
		var inputPer1M, outputPer1M float64

		if err := rows.Scan(&provider, &model, &inputPer1M, &outputPer1M); err != nil {
			fmt.Printf("⚠️ Failed to scan pricing row: %v\n", err)
			continue
		}

		// Store with provider:model key for exact lookups
		key := fmt.Sprintf("%s:%s", provider, model)
		c.pricing[key] = ModelPricing{
			Provider:          provider,
			Model:             model,
			InputPer1MTokens:  inputPer1M,
			OutputPer1MTokens: outputPer1M,
		}
		count++
	}

	fmt.Printf("✅ Loaded %d model prices from database\n", count)
}

// defaultPricing returns the current LLM pricing (as of Nov 2025)
func defaultPricing() map[string]ModelPricing {
	return map[string]ModelPricing{
		// OpenAI GPT-4 models
		"gpt-4-turbo": {
			Provider:          "openai",
			Model:             "gpt-4-turbo",
			InputPer1MTokens:  10.00,
			OutputPer1MTokens: 30.00,
		},
		"gpt-4": {
			Provider:          "openai",
			Model:             "gpt-4",
			InputPer1MTokens:  30.00,
			OutputPer1MTokens: 60.00,
		},
		"gpt-4o": {
			Provider:          "openai",
			Model:             "gpt-4o",
			InputPer1MTokens:  5.00,
			OutputPer1MTokens: 15.00,
		},
		"gpt-4o-mini": {
			Provider:          "openai",
			Model:             "gpt-4o-mini",
			InputPer1MTokens:  0.15,
			OutputPer1MTokens: 0.60,
		},
		// OpenAI GPT-3.5 models
		"gpt-3.5-turbo": {
			Provider:          "openai",
			Model:             "gpt-3.5-turbo",
			InputPer1MTokens:  0.50,
			OutputPer1MTokens: 1.50,
		},
		"gpt-3.5-turbo-16k": {
			Provider:          "openai",
			Model:             "gpt-3.5-turbo-16k",
			InputPer1MTokens:  3.00,
			OutputPer1MTokens: 4.00,
		},
		// Anthropic Claude models (pricing as of Jan 2026)
		// Claude 4.5 family
		"claude-opus-4-5": {
			Provider:          "anthropic",
			Model:             "claude-opus-4-5",
			InputPer1MTokens:  15.00,
			OutputPer1MTokens: 75.00,
		},
		"claude-haiku-4-5": {
			Provider:          "anthropic",
			Model:             "claude-haiku-4-5",
			InputPer1MTokens:  0.80,
			OutputPer1MTokens: 4.00,
		},
		"claude-sonnet-4-5": {
			Provider:          "anthropic",
			Model:             "claude-sonnet-4-5",
			InputPer1MTokens:  3.00,
			OutputPer1MTokens: 15.00,
		},
		// Claude 4 family
		"claude-opus-4-1": {
			Provider:          "anthropic",
			Model:             "claude-opus-4-1",
			InputPer1MTokens:  15.00,
			OutputPer1MTokens: 75.00,
		},
		"claude-opus-4": {
			Provider:          "anthropic",
			Model:             "claude-opus-4",
			InputPer1MTokens:  15.00,
			OutputPer1MTokens: 75.00,
		},
		"claude-sonnet-4": {
			Provider:          "anthropic",
			Model:             "claude-sonnet-4",
			InputPer1MTokens:  3.00,
			OutputPer1MTokens: 15.00,
		},
		// Claude 3.5 family
		"claude-3-5-haiku": {
			Provider:          "anthropic",
			Model:             "claude-3-5-haiku",
			InputPer1MTokens:  0.80,
			OutputPer1MTokens: 4.00,
		},
		// Claude 3 family
		"claude-3-opus": {
			Provider:          "anthropic",
			Model:             "claude-3-opus",
			InputPer1MTokens:  15.00,
			OutputPer1MTokens: 75.00,
		},
		"claude-3-sonnet": {
			Provider:          "anthropic",
			Model:             "claude-3-sonnet",
			InputPer1MTokens:  3.00,
			OutputPer1MTokens: 15.00,
		},
		"claude-3-haiku": {
			Provider:          "anthropic",
			Model:             "claude-3-haiku",
			InputPer1MTokens:  0.25,
			OutputPer1MTokens: 1.25,
		},
		// Google models
		"gemini-pro": {
			Provider:          "google",
			Model:             "gemini-pro",
			InputPer1MTokens:  0.50,
			OutputPer1MTokens: 1.50,
		},
		"gemini-pro-vision": {
			Provider:          "google",
			Model:             "gemini-pro-vision",
			InputPer1MTokens:  0.50,
			OutputPer1MTokens: 1.50,
		},
		"gemini-1.5-pro": {
			Provider:          "google",
			Model:             "gemini-1.5-pro",
			InputPer1MTokens:  3.50,
			OutputPer1MTokens: 10.50,
		},
		"gemini-1.5-flash": {
			Provider:          "google",
			Model:             "gemini-1.5-flash",
			InputPer1MTokens:  0.35,
			OutputPer1MTokens: 1.05,
		},
	}
}

// CalculateCost calculates the cost for a given model and token usage
func (c *Calculator) CalculateCost(provider, model string, inputTokens, outputTokens int) (float64, error) {
	// Ollama runs locally — no API cost.
	if provider == "ollama" {
		return 0, nil
	}

	// Try exact match first: provider:model
	key := fmt.Sprintf("%s:%s", provider, model)
	pricing, exists := c.pricing[key]

	if !exists {
		// Try normalized model name
		normalizedModel := c.normalizeModelName(model)
		key = fmt.Sprintf("%s:%s", provider, normalizedModel)
		pricing, exists = c.pricing[key]

		if !exists {
			// Try without provider (fallback)
			pricing, exists = c.pricing[normalizedModel]
			if !exists {
				return 0, fmt.Errorf("pricing not found for model: %s (provider: %s)", model, provider)
			}
		}
	}

	// Calculate cost
	inputCost := (float64(inputTokens) / 1_000_000) * pricing.InputPer1MTokens
	outputCost := (float64(outputTokens) / 1_000_000) * pricing.OutputPer1MTokens
	totalCost := inputCost + outputCost

	return totalCost, nil
}

// normalizeModelName removes version suffixes and normalizes model names
func (c *Calculator) normalizeModelName(model string) string {
	// Remove common version suffixes
	model = strings.TrimSuffix(model, "-0613")
	model = strings.TrimSuffix(model, "-1106")
	model = strings.TrimSuffix(model, "-0125")
	model = strings.TrimSuffix(model, "-0314")
	model = strings.TrimSuffix(model, "-0301")
	model = strings.TrimSuffix(model, "-20240229")

	// Remove date-based versions
	if idx := strings.LastIndex(model, "-202"); idx > 0 {
		model = model[:idx]
	}

	return model
}

// GetPricing returns the pricing for a specific model
func (c *Calculator) GetPricing(provider, model string) (*ModelPricing, error) {
	normalizedModel := c.normalizeModelName(model)

	pricing, exists := c.pricing[normalizedModel]
	if !exists {
		key := fmt.Sprintf("%s/%s", provider, normalizedModel)
		pricing, exists = c.pricing[key]
		if !exists {
			return nil, fmt.Errorf("pricing not found for model: %s", model)
		}
	}

	return &pricing, nil
}

// ListPricing returns all available pricing
func (c *Calculator) ListPricing() map[string]ModelPricing {
	return c.pricing
}

// EstimateCost estimates cost before making a request based on an input token count.
// Assumes output = 2x input when max_tokens is not known.
// Ollama (local) always returns 0.
func (c *Calculator) EstimateCost(provider, model string, estimatedInputTokens int) (float64, error) {
	if provider == "ollama" {
		return 0, nil
	}
	estimatedOutputTokens := estimatedInputTokens * 2 // Conservative when no max_tokens hint
	return c.CalculateCost(provider, model, estimatedInputTokens, estimatedOutputTokens)
}

// EstimateCostForRequest estimates cost using real prompt size and an optional
// max_tokens hint from the request body.
//
//   - inputTokens:   tokens in the prompt (use EstimatePromptTokens)
//   - maxTokens:     client-supplied cap from req.MaxTokens (may be nil)
//
// Output-token budget selection:
//   - If maxTokens is provided, it is used as the output budget.
//   - Otherwise, output is estimated as 2× input, clamped to [100, 2000]
//     so neither tiny prompts under-estimate nor huge documents massively
//     over-estimate the post-call cost.
//
// Ollama (local) always returns 0.
func (c *Calculator) EstimateCostForRequest(provider, model string, inputTokens int, maxTokens *int) (float64, error) {
	if provider == "ollama" {
		return 0, nil
	}

	var outputTokens int
	if maxTokens != nil && *maxTokens > 0 {
		outputTokens = *maxTokens
	} else {
		outputTokens = inputTokens * 2
		if outputTokens < 100 {
			outputTokens = 100
		}
		if outputTokens > 2000 {
			outputTokens = 2000
		}
	}

	return c.CalculateCost(provider, model, inputTokens, outputTokens)
}
