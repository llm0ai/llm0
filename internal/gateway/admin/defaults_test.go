package admin

import "testing"

func strPtr(s string) *string { return &s }

func TestValidateDefaultsRequest_AllowsAllFieldsUnset(t *testing.T) {
	// A PATCH with nothing set ("leave everything unchanged") is valid —
	// callers use this to touch just one field.
	req := updateDefaultsRequest{}
	if err := req.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDefaultsRequest_RejectsUnknownBehavior(t *testing.T) {
	req := updateDefaultsRequest{OnLimitBehavior: strPtr("explode")}
	if err := req.validate(); err == nil {
		t.Fatal("expected error for unknown on_limit_behavior, got nil")
	}
}

func TestValidateDefaultsRequest_DowngradeRequiresModelInSameRequest(t *testing.T) {
	req := updateDefaultsRequest{OnLimitBehavior: strPtr("downgrade")}
	if err := req.validate(); err == nil {
		t.Fatal("expected error when setting behavior=downgrade without downgrade_model")
	}
}

func TestValidateDefaultsRequest_AcceptsValidDowngrade(t *testing.T) {
	req := updateDefaultsRequest{
		OnLimitBehavior: strPtr("downgrade"),
		DowngradeModel:  strPtr("gpt-4o-mini"),
	}
	if err := req.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDefaultsRequest_AcceptsBlockWithoutDowngradeModel(t *testing.T) {
	req := updateDefaultsRequest{OnLimitBehavior: strPtr("block")}
	if err := req.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
