package admin

import (
	"encoding/json"
	"testing"

	"github.com/llm0ai/llm0/internal/shared/models"
)

// TestCustomerTier_JSONUsesSnakeCase guards against the response admin/tiers.go
// sends back losing its json tags again — models.CustomerTier embeds
// LimitSpec, and Go silently falls back to the exported Go field name
// (e.g. "Slug") for any field missing a json tag, which broke
// scripts/admin_smoke.sh's list-tiers step.
func TestCustomerTier_JSONUsesSnakeCase(t *testing.T) {
	tier := models.CustomerTier{Slug: "pro"}
	out, err := json.Marshal(tier)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := decoded["slug"]; !ok {
		t.Fatalf("expected lowercase %q key in %s", "slug", out)
	}
	if _, ok := decoded["Slug"]; ok {
		t.Fatalf("found capitalized %q key (missing json tag) in %s", "Slug", out)
	}
}

func TestValidateTierRequest_RequiresSlug(t *testing.T) {
	req := upsertTierRequest{Slug: ""}
	if err := req.validate(); err == nil {
		t.Fatal("expected error for empty slug, got nil")
	}
}

func TestValidateTierRequest_RejectsUnknownBehavior(t *testing.T) {
	req := upsertTierRequest{Slug: "pro", OnLimitBehavior: "explode"}
	if err := req.validate(); err == nil {
		t.Fatal("expected error for unknown on_limit_behavior, got nil")
	}
}

func TestValidateTierRequest_DowngradeRequiresModel(t *testing.T) {
	req := upsertTierRequest{Slug: "pro", OnLimitBehavior: "downgrade"}
	if err := req.validate(); err == nil {
		t.Fatal("expected error when behavior=downgrade but downgrade_model is empty")
	}
}

func TestValidateTierRequest_AcceptsValidBlock(t *testing.T) {
	req := upsertTierRequest{Slug: "free"} // OnLimitBehavior defaults to "block"
	if err := req.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.OnLimitBehavior != "block" {
		t.Fatalf("OnLimitBehavior = %q, want default %q", req.OnLimitBehavior, "block")
	}
}

func TestValidateTierRequest_AcceptsValidDowngrade(t *testing.T) {
	model := "gpt-4o-mini"
	req := upsertTierRequest{Slug: "pro", OnLimitBehavior: "downgrade", DowngradeModel: &model}
	if err := req.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
