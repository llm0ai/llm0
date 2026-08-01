package admin

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/shared/database"
)

// projectsHandler serves /v1/admin/projects and /v1/admin/projects/:id.
type projectsHandler struct {
	db *database.DB
}

func newProjectsHandler(db *database.DB) *projectsHandler {
	return &projectsHandler{db: db}
}

// createProjectRequest is the body for POST /v1/admin/projects.
type createProjectRequest struct {
	UserID        uuid.UUID `json:"user_id" binding:"required"`
	Name          string    `json:"name" binding:"required"`
	MonthlyCapUSD *float64  `json:"monthly_cap_usd"`
}

// list handles GET /v1/admin/projects?user_id=<uuid>. user_id is optional —
// omitted, it returns every project (the dashboard's "all my org's
// projects" view doesn't exist yet, so the cloud backend filters).
func (h *projectsHandler) list(c *gin.Context) {
	var userID *uuid.UUID
	if raw := c.Query("user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_user_id"})
			return
		}
		userID = &id
	}

	projects, err := h.db.ListProjects(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// create handles POST /v1/admin/projects.
func (h *projectsHandler) create(c *gin.Context) {
	var req createProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	project, err := h.db.CreateProject(c.Request.Context(), req.UserID, req.Name, req.MonthlyCapUSD)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, project)
}

// get handles GET /v1/admin/projects/:id.
func (h *projectsHandler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	project, err := h.db.GetProject(c.Request.Context(), id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "project_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, project)
}

// updateProjectRequest is the body for PATCH /v1/admin/projects/:id. Every
// field is optional — an omitted field is left unchanged.
type updateProjectRequest struct {
	Name                 *string  `json:"name"`
	MonthlyCapUSD        *float64 `json:"monthly_cap_usd"`
	CacheEnabled         *bool    `json:"cache_enabled"`
	SemanticCacheEnabled *bool    `json:"semantic_cache_enabled"`
	SemanticThreshold    *float64 `json:"semantic_threshold"`
	CacheTTLSeconds      *int     `json:"cache_ttl_seconds"`
	IsActive             *bool    `json:"is_active"`
}

// update handles PATCH /v1/admin/projects/:id.
func (h *projectsHandler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_project_id"})
		return
	}

	var req updateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	project, err := h.db.UpdateProject(c.Request.Context(), id, &database.ProjectPatch{
		Name:                 req.Name,
		MonthlyCapUSD:        req.MonthlyCapUSD,
		CacheEnabled:         req.CacheEnabled,
		SemanticCacheEnabled: req.SemanticCacheEnabled,
		SemanticThreshold:    req.SemanticThreshold,
		CacheTTLSeconds:      req.CacheTTLSeconds,
		IsActive:             req.IsActive,
	})
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "project_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, project)
}
