package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newAuthedTestRouter builds a minimal router with RequireAdminToken in
// front of a handler that just returns 200, so tests can focus on the
// middleware's accept/reject decision.
func newAuthedTestRouter(token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireAdminToken(token))
	router.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

func doProbe(t *testing.T, router *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRequireAdminToken_ValidTokenPasses(t *testing.T) {
	router := newAuthedTestRouter("s3cret")
	rec := doProbe(t, router, "Bearer s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireAdminToken_WrongTokenRejected(t *testing.T) {
	router := newAuthedTestRouter("s3cret")
	rec := doProbe(t, router, "Bearer wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAdminToken_MissingHeaderRejected(t *testing.T) {
	router := newAuthedTestRouter("s3cret")
	rec := doProbe(t, router, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAdminToken_EmptyConfiguredTokenAlwaysRejects(t *testing.T) {
	// A misconfigured (empty) ADMIN_TOKEN must never be satisfiable by an
	// empty Authorization header — see the comment in middleware.go.
	router := newAuthedTestRouter("")
	rec := doProbe(t, router, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (empty token must never authenticate)", rec.Code)
	}

	rec = doProbe(t, router, "Bearer ")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAdminToken_MalformedSchemeRejected(t *testing.T) {
	router := newAuthedTestRouter("s3cret")
	// No "Bearer " prefix means TrimPrefix is a no-op, so this only passes
	// if the whole header equals the token — assert it doesn't for a
	// scheme-less header carrying the right token.
	rec := doProbe(t, router, "s3cret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (missing Bearer scheme)", rec.Code)
	}
}
