package handlers

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// HealthHandler handles health check requests
type HealthHandler struct {
	db    *database.DB
	redis *redis.Client
}

// NewHealthHandler creates a new health handler
func NewHealthHandler(db *database.DB, redis *redis.Client) *HealthHandler {
	return &HealthHandler{
		db:    db,
		redis: redis,
	}
}

// HealthCheck handles GET /health
func (h *HealthHandler) HealthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	status := gin.H{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"service":   "llm0",
	}

	// Check database
	if err := h.db.HealthCheck(ctx); err != nil {
		status["database"] = "unhealthy"
		status["database_error"] = err.Error()
		status["status"] = "degraded"
	} else {
		status["database"] = "healthy"
	}

	// Check Redis
	if err := h.redis.HealthCheck(ctx); err != nil {
		status["redis"] = "unhealthy"
		status["redis_error"] = err.Error()
		status["status"] = "degraded"
	} else {
		status["redis"] = "healthy"
	}

	statusCode := 200
	if status["status"] == "degraded" {
		statusCode = 503
	}

	c.JSON(statusCode, status)
}

// ReadyCheck handles GET /ready (for Kubernetes readiness probes)
func (h *HealthHandler) ReadyCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
	defer cancel()

	// Quick checks - fail fast
	dbErr := h.db.HealthCheck(ctx)
	redisErr := h.redis.HealthCheck(ctx)

	if dbErr != nil || redisErr != nil {
		c.JSON(503, gin.H{
			"ready":    false,
			"database": dbErr == nil,
			"redis":    redisErr == nil,
		})
		return
	}

	c.JSON(200, gin.H{
		"ready": true,
	})
}

// LiveCheck handles GET /live (for Kubernetes liveness probes)
func (h *HealthHandler) LiveCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"alive": true,
	})
}
