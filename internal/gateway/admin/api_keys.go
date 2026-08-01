package admin

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/shared/database"
)

// apiKeysHandler serves /v1/admin/projects/:id/api-keys and its :key_id
// sub-resource.
type apiKeysHandler struct {
	db *database.DB
}

func newAPIKeysHandler(db *database.DB) *apiKeysHandler {
	return &apiKeysHandler{db: db}
}

// createAPIKeyRequest is the body for POST /v1/admin/projects/:id/api-keys.
type createAPIKeyRequest struct {
	Name               string `json:"name" binding:"required"`
	RateLimitPerMinute int    `json:"rate_limit_per_minute"`
}

// list handles GET /v1/admin/projects/:id/api-keys.
func (h *apiKeysHandler) list(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	keys, err := h.db.ListAPIKeys(c.Request.Context(), projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": keys})
}

// create handles POST /v1/admin/projects/:id/api-keys. The raw key is
// returned exactly once, here — the database only ever stores its bcrypt
// hash, so losing this response means generating a new key, not recovering
// this one.
func (h *apiKeysHandler) create(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	var req createAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	rawKey, key, err := h.db.CreateAPIKey(c.Request.Context(), projectID, req.Name, req.RateLimitPerMinute)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create_failed", "message": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"api_key": rawKey,
		"key":     key,
	})
}

// updateAPIKeyRequest is the body for PATCH .../api-keys/:key_id. Both
// fields are optional — an omitted field is left unchanged.
type updateAPIKeyRequest struct {
	RateLimitPerMinute *int  `json:"rate_limit_per_minute"`
	IsActive           *bool `json:"is_active"`
}

// update handles PATCH /v1/admin/projects/:id/api-keys/:key_id. The :id
// segment is validated against the key's actual project_id so a copy-paste
// mistake in the caller can't silently edit the wrong project's key.
func (h *apiKeysHandler) update(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}
	keyID, err := uuid.Parse(c.Param("key_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_key_id"})
		return
	}

	var req updateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	key, err := h.db.UpdateAPIKey(c.Request.Context(), keyID, req.RateLimitPerMinute, req.IsActive)
	if err == sql.ErrNoRows || (err == nil && key.ProjectID != projectID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "api_key_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, key)
}
