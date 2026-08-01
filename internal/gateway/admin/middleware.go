package admin

import (
	"crypto/subtle"
	"strings"

	"github.com/gin-gonic/gin"
)

// RequireAdminToken returns Gin middleware that authenticates every admin
// request against a single shared secret (ADMIN_TOKEN).
//
// This token is the SECOND layer of defense, not the only one. The first
// and primary layer is that this router is only ever bound to an
// internal-only listener — see cmd/gateway/main.go, which starts the admin
// API on its own port (ADMIN_LISTEN_ADDR) that production deployments never
// expose to the public internet. See plans/managed/07-deployment-and-ops.md
// §1a for the full rationale.
func RequireAdminToken(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		const scheme = "Bearer "

		header := c.GetHeader("Authorization")
		provided, hasScheme := strings.CutPrefix(header, scheme)

		// The scheme check, the empty check, and the length check below
		// all come before the constant-time compare — none of them depend
		// on the token's contents, so they can't leak anything about it.
		// An unconfigured (empty) ADMIN_TOKEN can therefore never be
		// satisfied, even by an empty/missing header.
		valid := hasScheme && provided != "" && token != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1

		if !valid {
			c.JSON(401, gin.H{
				"error":   "invalid_admin_token",
				"message": "Authorization header must be 'Bearer <ADMIN_TOKEN>'",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}
