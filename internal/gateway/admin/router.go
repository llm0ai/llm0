// Package admin implements the gateway's control plane: CRUD over
// projects, API keys, and (soon) tiers/defaults/provider-keys. It exists
// alongside the public data plane (internal/gateway/handlers) but is never
// mounted on the same listener — cmd/gateway/main.go runs it on its own
// http.Server and port so it can be network-isolated independently of the
// public-facing API. See plans/managed/06-milestones-and-roadmap.md (M0)
// and plans/managed/07-deployment-and-ops.md §1a for the full rationale.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/llm0ai/llm0/internal/shared/database"
)

// NewRouter builds the Gin engine for the admin control plane. Every route
// under /v1/admin requires ADMIN_TOKEN; /health does not, so an internal
// load balancer or orchestrator can probe liveness without a credential.
func NewRouter(db *database.DB, adminToken string) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "llm0-admin"})
	})

	projects := newProjectsHandler(db)
	apiKeys := newAPIKeysHandler(db)

	v1 := router.Group("/v1/admin")
	v1.Use(RequireAdminToken(adminToken))
	{
		v1.GET("/projects", projects.list)
		v1.POST("/projects", projects.create)
		v1.GET("/projects/:id", projects.get)
		v1.PATCH("/projects/:id", projects.update)

		v1.GET("/projects/:id/api-keys", apiKeys.list)
		v1.POST("/projects/:id/api-keys", apiKeys.create)
		v1.PATCH("/projects/:id/api-keys/:key_id", apiKeys.update)
	}

	return router
}
