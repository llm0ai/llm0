package admin

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// defaultsHandler serves /v1/admin/projects/:id/defaults — the per-customer
// DEFAULT limits a project applies to every customer that doesn't have a
// more specific tier (see internal/shared/database/resolver.go). Backed by
// the projects.default_* columns; this is the OSS equivalent of "cap every
// user of my SaaS at $5/day without inserting a row per user."
type defaultsHandler struct {
	db *database.DB
}

func newDefaultsHandler(db *database.DB) *defaultsHandler {
	return &defaultsHandler{db: db}
}

// get handles GET /v1/admin/projects/:id/defaults.
func (h *defaultsHandler) get(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	defaults, err := h.db.GetProjectDefaults(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project_not_found", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, defaults)
}

// updateDefaultsRequest is the body for PATCH .../defaults. Every field is
// optional and, unlike tiers, this is a genuine partial update (nil = leave
// unchanged) — matching SetProjectDefaults' COALESCE semantics and
// scripts/manage_project_defaults.sh's "blank = leave unchanged" prompts.
type updateDefaultsRequest struct {
	DailySpendLimitUSD   *float64 `json:"default_daily_spend_limit_usd"`
	MonthlySpendLimitUSD *float64 `json:"default_monthly_spend_limit_usd"`
	PerRequestMaxUSD     *float64 `json:"default_per_request_max_usd"`
	RequestsPerMinute    *int     `json:"default_requests_per_minute"`
	RequestsPerHour      *int     `json:"default_requests_per_hour"`
	RequestsPerDay       *int     `json:"default_requests_per_day"`
	OnLimitBehavior      *string  `json:"default_on_limit_behavior"`
	DowngradeModel       *string  `json:"default_downgrade_model"`
}

// validate rejects an unrecognized OnLimitBehavior, and requires
// DowngradeModel to be set in the SAME request when OnLimitBehavior is
// being changed to "downgrade" — a project can't be left in a state where
// it's configured to downgrade to no model. A pure function, unit tested
// directly in defaults_test.go.
func (r *updateDefaultsRequest) validate() error {
	if r.OnLimitBehavior == nil {
		return nil
	}
	switch models.LimitBehavior(*r.OnLimitBehavior) {
	case models.LimitBehaviorBlock, models.LimitBehaviorWarn:
		return nil
	case models.LimitBehaviorDowngrade:
		if r.DowngradeModel == nil || *r.DowngradeModel == "" {
			return fmt.Errorf("downgrade_model is required in the same request when setting on_limit_behavior to 'downgrade'")
		}
		return nil
	default:
		return fmt.Errorf("on_limit_behavior must be one of: block, downgrade, warn (got %q)", *r.OnLimitBehavior)
	}
}

// update handles PATCH /v1/admin/projects/:id/defaults.
func (h *defaultsHandler) update(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	var req updateDefaultsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}
	if err := req.validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	patch := &database.ProjectDefaultLimits{
		DailySpendLimitUSD:   req.DailySpendLimitUSD,
		MonthlySpendLimitUSD: req.MonthlySpendLimitUSD,
		PerRequestMaxUSD:     req.PerRequestMaxUSD,
		RequestsPerMinute:    req.RequestsPerMinute,
		RequestsPerHour:      req.RequestsPerHour,
		RequestsPerDay:       req.RequestsPerDay,
		OnLimitBehavior:      req.OnLimitBehavior,
		DowngradeModel:       req.DowngradeModel,
	}
	if err := h.db.SetProjectDefaults(c.Request.Context(), projectID, patch); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "project_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update_failed", "message": err.Error()})
		return
	}

	defaults, err := h.db.GetProjectDefaults(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, defaults)
}

// clear handles DELETE /v1/admin/projects/:id/defaults — wipes every
// default_* column back to NULL (behavior back to "block"), same as
// scripts/manage_project_defaults.sh's "clear" command.
func (h *defaultsHandler) clear(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	if err := h.db.ClearProjectDefaults(c.Request.Context(), projectID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "project_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "clear_failed", "message": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
