package models

import "testing"

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }
func strPtr(v string) *string     { return &v }

func TestLimitSpec_IsEmpty(t *testing.T) {
	cases := []struct {
		name string
		s    *LimitSpec
		want bool
	}{
		{"nil receiver", nil, true},
		{"zero value", &LimitSpec{}, true},
		{"only behavior set (no caps)", &LimitSpec{OnLimitBehavior: LimitBehaviorBlock}, true},
		{"daily set", &LimitSpec{DailySpendLimitUSD: floatPtr(1)}, false},
		{"monthly set", &LimitSpec{MonthlySpendLimitUSD: floatPtr(1)}, false},
		{"per-request set", &LimitSpec{PerRequestMaxUSD: floatPtr(1)}, false},
		{"rpm set", &LimitSpec{RequestsPerMinute: intPtr(5)}, false},
		{"model limits set", &LimitSpec{ModelLimits: ModelLimits{"gpt-4o": intPtr(10)}}, false},
		{"label limits set", &LimitSpec{LabelLimits: LabelLimits{"feature:chat": 1}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.IsEmpty(); got != tc.want {
				t.Fatalf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLimitSpec_HasCostLimit(t *testing.T) {
	if (&LimitSpec{}).HasCostLimit() {
		t.Fatal("empty spec should not have a cost limit")
	}
	if !(&LimitSpec{DailySpendLimitUSD: floatPtr(1)}).HasCostLimit() {
		t.Fatal("daily-cap spec should have a cost limit")
	}
	if !(&LimitSpec{PerRequestMaxUSD: floatPtr(0.05)}).HasCostLimit() {
		t.Fatal("per-request-cap spec should have a cost limit")
	}
}

func TestLimitSpec_HasRequestLimit(t *testing.T) {
	if (&LimitSpec{}).HasRequestLimit() {
		t.Fatal("empty spec should not have a request limit")
	}
	if !(&LimitSpec{RequestsPerHour: intPtr(60)}).HasRequestLimit() {
		t.Fatal("rph spec should have a request limit")
	}
}

func TestLimitSpec_ModelAndLabel(t *testing.T) {
	s := &LimitSpec{
		ModelLimits: ModelLimits{"gpt-4o": intPtr(10), "gpt-4o-mini": nil},
		LabelLimits: LabelLimits{"feature:chat": 100},
	}

	if !s.HasModelLimit("gpt-4o") {
		t.Fatal("expected HasModelLimit(gpt-4o) = true")
	}
	if !s.HasModelLimit("gpt-4o-mini") {
		t.Fatal("expected HasModelLimit(gpt-4o-mini) = true (configured even if nil)")
	}
	if s.HasModelLimit("claude-3-5-sonnet") {
		t.Fatal("expected HasModelLimit(claude) = false")
	}

	if v, ok := s.GetModelLimit("gpt-4o"); !ok || v != 10 {
		t.Fatalf("GetModelLimit(gpt-4o) = (%d, %v), want (10, true)", v, ok)
	}
	// Configured-but-nil means "no daily cap for this model".
	if _, ok := s.GetModelLimit("gpt-4o-mini"); ok {
		t.Fatal("GetModelLimit(gpt-4o-mini) should return ok=false when limit is nil")
	}

	if v, ok := s.GetLabelLimit("feature:chat"); !ok || v != 100 {
		t.Fatalf("GetLabelLimit = (%d, %v), want (100, true)", v, ok)
	}
}

func TestCachedAPIKey_ProjectDefaultLimitSpec(t *testing.T) {
	t.Run("nil key", func(t *testing.T) {
		var k *CachedAPIKey
		if got := k.ProjectDefaultLimitSpec(); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("no defaults set", func(t *testing.T) {
		k := &CachedAPIKey{
			DefaultOnLimitBehavior: strPtr("block"), // SQL default; alone shouldn't count
		}
		if got := k.ProjectDefaultLimitSpec(); got != nil {
			t.Fatalf("expected nil (only on_limit_behavior set), got %+v", got)
		}
	})

	t.Run("daily default set", func(t *testing.T) {
		k := &CachedAPIKey{
			DefaultDailySpendLimitUSD: floatPtr(2.50),
			DefaultOnLimitBehavior:    strPtr("block"),
		}
		spec := k.ProjectDefaultLimitSpec()
		if spec == nil {
			t.Fatal("expected non-nil spec")
		}
		if spec.DailySpendLimitUSD == nil || *spec.DailySpendLimitUSD != 2.50 {
			t.Fatalf("daily = %v, want 2.50", spec.DailySpendLimitUSD)
		}
		if spec.OnLimitBehavior != LimitBehaviorBlock {
			t.Fatalf("OnLimitBehavior = %q, want %q", spec.OnLimitBehavior, LimitBehaviorBlock)
		}
	})

	t.Run("downgrade default", func(t *testing.T) {
		k := &CachedAPIKey{
			DefaultMonthlySpendLimitUSD: floatPtr(20),
			DefaultOnLimitBehavior:      strPtr("downgrade"),
			DefaultDowngradeModel:       strPtr("gpt-4o-mini"),
		}
		spec := k.ProjectDefaultLimitSpec()
		if spec == nil {
			t.Fatal("expected non-nil spec")
		}
		if spec.OnLimitBehavior != LimitBehaviorDowngrade {
			t.Fatalf("OnLimitBehavior = %q, want %q", spec.OnLimitBehavior, LimitBehaviorDowngrade)
		}
		if spec.DowngradeModel == nil || *spec.DowngradeModel != "gpt-4o-mini" {
			t.Fatalf("downgrade_model = %v", spec.DowngradeModel)
		}
	})
}

// Compile-time check: CustomerLimit's promoted methods (Has*Limit etc.) come
// from the embedded LimitSpec. If the embedding accidentally regresses to a
// nested field this test still compiles, but the documentation here exists
// to catch reviewers' eyes during the refactor.
func TestCustomerLimit_PromotesLimitSpec(t *testing.T) {
	cl := &CustomerLimit{LimitSpec: LimitSpec{DailySpendLimitUSD: floatPtr(1)}}
	if !cl.HasCostLimit() {
		t.Fatal("CustomerLimit should promote HasCostLimit via embedded LimitSpec")
	}
	if cl.DailySpendLimitUSD == nil || *cl.DailySpendLimitUSD != 1 {
		t.Fatal("CustomerLimit should expose DailySpendLimitUSD via embedded LimitSpec")
	}
}

// Same compile-time/behavioral check for CustomerTier.
func TestCustomerTier_PromotesLimitSpec(t *testing.T) {
	tier := &CustomerTier{
		Slug:      "pro",
		LimitSpec: LimitSpec{MonthlySpendLimitUSD: floatPtr(50), OnLimitBehavior: LimitBehaviorDowngrade, DowngradeModel: strPtr("gpt-4o-mini")},
	}
	if !tier.HasCostLimit() {
		t.Fatal("CustomerTier should promote HasCostLimit via embedded LimitSpec")
	}
	if tier.OnLimitBehavior != LimitBehaviorDowngrade {
		t.Fatal("CustomerTier should expose OnLimitBehavior via embedded LimitSpec")
	}
}
