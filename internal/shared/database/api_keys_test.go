package database

import (
	"strings"
	"testing"
)

func TestGenerateRawAPIKey_Format(t *testing.T) {
	key, err := generateRawAPIKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const prefix = "llm0_live_"
	if !strings.HasPrefix(key, prefix) {
		t.Fatalf("key %q missing prefix %q", key, prefix)
	}

	// 32 random bytes hex-encoded = 64 chars, plus the prefix.
	wantLen := len(prefix) + 64
	if len(key) != wantLen {
		t.Fatalf("key length = %d, want %d (key=%q)", len(key), wantLen, key)
	}

	// apiKeyPrefixLen (used to slice the "shown in the dashboard" prefix)
	// must never exceed the full key length, or CreateAPIKey would panic.
	if apiKeyPrefixLen > len(key) {
		t.Fatalf("apiKeyPrefixLen (%d) exceeds key length (%d)", apiKeyPrefixLen, len(key))
	}
}

func TestGenerateRawAPIKey_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		key, err := generateRawAPIKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[key] {
			t.Fatalf("generated duplicate key: %q", key)
		}
		seen[key] = true
	}
}
