package tls

import (
	"crypto/tls"
	"fmt"

	"github.com/llm0ai/llm0/internal/shared/config"
)

// CreateOptimizedTLSConfig creates a TLS 1.3 configuration optimized for performance
// Based on patterns from rate_limiter_go
func CreateOptimizedTLSConfig(cfg *config.Config) (*tls.Config, error) {
	if !cfg.TLSEnabled {
		return nil, nil
	}

	// Create LRU session cache for connection reuse
	sessionCache := tls.NewLRUClientSessionCache(cfg.TLSSessionCacheSize)

	tlsConfig := &tls.Config{
		// Force TLS 1.3 for optimal performance
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,

		// TLS 1.3 cipher suites (only these are available)
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},

		// Performance optimizations
		PreferServerCipherSuites: true,
		SessionTicketsDisabled:   false, // Enable for faster handshakes

		// Session caching for connection reuse
		ClientSessionCache: sessionCache,

		// Curve preferences (X25519 is fastest)
		CurvePreferences: []tls.CurveID{
			tls.X25519, // Fastest curve
			tls.CurveP256,
			tls.CurveP384,
		},

		// Enable session resumption
		SessionTicketKey: [32]byte{}, // Filled by Go runtime
	}

	fmt.Printf("✅ TLS 1.3 configuration created\n")
	fmt.Printf("   Session cache size: %d\n", cfg.TLSSessionCacheSize)
	fmt.Printf("   Cipher suites: AES-GCM, ChaCha20-Poly1305\n")
	fmt.Printf("   Curve preference: X25519 (fastest)\n")

	return tlsConfig, nil
}
