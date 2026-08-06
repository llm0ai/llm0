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

// tiersHandler serves /v1/admin/projects/:id/tiers and its :slug
// sub-resource. Tiers are owner-named "plans" a customer attaches to via
// the X-Customer-Tier header; see internal/shared/database/customer_tiers.go
// for the resolution/caching rules this handler sits on top of.
type tiersHandler struct {
	db *database.DB
}

func newTiersHandler(db *database.DB) *tiersHandler {
	return &tiersHandler{db: db}
}

// upsertTierRequest is the body for POST /v1/admin/projects/:id/tiers. It
// mirrors scripts/manage_tiers.sh: a full replace, not a partial patch — an
// omitted cap field means "no limit on that axis" (NULL), same as leaving a
// prompt blank in the script. Callers that only want to change one field on
// an existing tier must resend the others they want to keep.
type upsertTierRequest struct {
	Slug string `json:"slug" binding:"required"`

	DailySpendLimitUSD   *float64 `json:"daily_spend_limit_usd"`
	MonthlySpendLimitUSD *float64 `json:"monthly_spend_limit_usd"`
	PerRequestMaxUSD     *float64 `json:"per_request_max_usd"`

	RequestsPerMinute *int `json:"requests_per_minute"`
	RequestsPerHour   *int `json:"requests_per_hour"`
	RequestsPerDay    *int `json:"requests_per_day"`

	// OnLimitBehavior defaults to "block" when omitted, matching the script.
	OnLimitBehavior string  `json:"on_limit_behavior"`
	DowngradeModel  *string `json:"downgrade_model"`
}

// validate checks the request is internally consistent and fills in the
// default OnLimitBehavior. It's a pure function (no I/O) so it's unit
// tested directly in tiers_test.go rather than through HTTP.
func (r *upsertTierRequest) validate() error {
	if r.Slug == "" {
		return fmt.Errorf("slug is required")
	}
	if r.OnLimitBehavior == "" {
		r.OnLimitBehavior = string(models.LimitBehaviorBlock)
	}
	switch models.LimitBehavior(r.OnLimitBehavior) {
	case models.LimitBehaviorBlock, models.LimitBehaviorWarn:
		// no extra fields required
	case models.LimitBehaviorDowngrade:
		if r.DowngradeModel == nil || *r.DowngradeModel == "" {
			return fmt.Errorf("downgrade_model is required when on_limit_behavior is 'downgrade'")
		}
	default:
		return fmt.Errorf("on_limit_behavior must be one of: block, downgrade, warn (got %q)", r.OnLimitBehavior)
	}
	return nil
}

// list handles GET /v1/admin/projects/:id/tiers.
func (h *tiersHandler) list(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	tiers, err := h.db.ListCustomerTiers(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tiers": tiers})
}

// upsert handles POST /v1/admin/projects/:id/tiers. Creates the tier if
// its slug is new for this project, otherwise fully replaces it — see
// upsertTierRequest's doc comment.
func (h *tiersHandler) upsert(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	var req upsertTierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}
	if err := req.validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	tier := &models.CustomerTier{
		ProjectID: projectID,
		Slug:      req.Slug,
		LimitSpec: models.LimitSpec{
			DailySpendLimitUSD:   req.DailySpendLimitUSD,
			MonthlySpendLimitUSD: req.MonthlySpendLimitUSD,
			PerRequestMaxUSD:     req.PerRequestMaxUSD,
			RequestsPerMinute:    req.RequestsPerMinute,
			RequestsPerHour:      req.RequestsPerHour,
			RequestsPerDay:       req.RequestsPerDay,
			OnLimitBehavior:      models.LimitBehavior(req.OnLimitBehavior),
			DowngradeModel:       req.DowngradeModel,
		},
	}
	if err := h.db.UpsertCustomerTier(c.Request.Context(), tier); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upsert_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tier)
}

// delete handles DELETE /v1/admin/projects/:id/tiers/:slug. Customers
// carrying this slug fall through to the project default on their next
// request, same as scripts/manage_tiers.sh delete.
func (h *tiersHandler) delete(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}
	slug := c.Param("slug")

	err = h.db.DeleteCustomerTier(c.Request.Context(), projectID, slug)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "tier_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete_failed", "message": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
