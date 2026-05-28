package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// Middleware handles API key authentication for Gateway requests
type Middleware struct {
	validator *Validator
}

// NewMiddleware creates a new auth middleware
func NewMiddleware(validator *Validator) *Middleware {
	return &Middleware{
		validator: validator,
	}
}

// RequireAPIKey is a Gin middleware that validates API keys
func (m *Middleware) RequireAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()

		// Extract API key from Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(401, gin.H{
				"error":   "missing_authorization",
				"message": "Authorization header is required",
			})
			c.Abort()
			return
		}

		// Parse "Bearer <api_key>" format
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.JSON(401, gin.H{
				"error":   "invalid_authorization_format",
				"message": "Authorization header must be 'Bearer <api_key>'",
			})
			c.Abort()
			return
		}

		apiKey := parts[1]

		// Validate API key (with Redis caching)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
		defer cancel()

		cachedKey, err := m.validator.ValidateAPIKey(ctx, apiKey)
		if err != nil {
			c.JSON(401, gin.H{
				"error":   "invalid_api_key",
				"message": err.Error(),
			})
			c.Abort()
			return
		}

		// Store validated key info in context for downstream handlers
		c.Set("api_key", cachedKey)
		c.Set("project_id", cachedKey.ProjectID)
		c.Set("key_id", cachedKey.KeyID)

		authLatency := time.Since(startTime).Milliseconds()
		fmt.Printf("🔑 API key validated in %dms (project=%s, rate_limit=%d/min)\n",
			authLatency, cachedKey.ProjectID, cachedKey.RateLimitPerMinute)

		// Update last_used_at in background (don't block request)
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer bgCancel()
			if err := m.validator.UpdateLastUsed(bgCtx, cachedKey.KeyID); err != nil {
				fmt.Printf("⚠️ Failed to update last_used_at: %v\n", err)
			}
		}()

		c.Next()
	}
}

// GetAPIKey retrieves the validated API key from context
func GetAPIKey(c *gin.Context) (*models.CachedAPIKey, bool) {
	value, exists := c.Get("api_key")
	if !exists {
		return nil, false
	}
	cachedKey, ok := value.(*models.CachedAPIKey)
	return cachedKey, ok
}
