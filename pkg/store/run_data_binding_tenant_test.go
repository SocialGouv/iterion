package store

import (
	"context"
	"errors"
	"testing"
)

// bindTenantObservation binds ctx to a present observation of h whose store
// recorded tenant.
func bindTenantObservation(t *testing.T, ctx context.Context, h RunDataHeader, tenant string) context.Context {
	t.Helper()
	o, err := CaptureRunObservation(h, tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindRunObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func boundTenant(t *testing.T, ctx context.Context) (string, bool) {
	t.Helper()
	b, ok := RunDataBindingFrom(ctx)
	if !ok {
		t.Fatal("ctx carries no binding")
	}
	return b.TenantID()
}

// TestRunDataBindingCarriesTheObservedTenant: binding an observation carries
// the tenant its store recorded, "" included, and CheckRunObservedTenant
// refuses any other canonical tenant. A header-only binding observes none and
// keeps the legacy answer.
func TestRunDataBindingCarriesTheObservedTenant(t *testing.T) {
	t.Parallel()
	h := RunDataHeader{RunID: "r", Format: ExactValueFormat, BirthID: "birth-a"}

	teamA := bindTenantObservation(t, t.Context(), h, "team-a")
	if tenant, observed := boundTenant(t, teamA); tenant != "team-a" || !observed {
		t.Fatalf("binding of an observation of \"team-a\" carries (%q, %t), want (\"team-a\", true)", tenant, observed)
	}
	if err := CheckRunObservedTenant(teamA, "team-a"); err != nil {
		t.Fatalf("observed \"team-a\", stored \"team-a\": %v", err)
	}
	for _, stored := range []string{"team-b", ""} {
		if err := CheckRunObservedTenant(teamA, stored); !errors.Is(err, ErrRunDataIdentityChanged) {
			t.Fatalf("observed \"team-a\", stored %q: %v, want ErrRunDataIdentityChanged", stored, err)
		}
	}

	none := bindTenantObservation(t, t.Context(), h, "")
	if tenant, observed := boundTenant(t, none); tenant != "" || !observed {
		t.Fatalf("binding of an observation of no tenant carries (%q, %t), want (\"\", true): \"\" is observed", tenant, observed)
	}
	if err := CheckRunObservedTenant(none, "team-a"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("observed \"\", stored \"team-a\": %v, want ErrRunDataIdentityChanged", err)
	}
	if err := CheckRunObservedTenant(none, ""); err != nil {
		t.Fatalf("observed \"\", stored \"\": %v", err)
	}

	o, err := CaptureRunObservation(h, "team-a", true)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := CaptureRunDataObservation(o)
	if err != nil {
		t.Fatal(err)
	}
	if tenant, observed := direct.TenantID(); direct.Header() != h || tenant != "team-a" || !observed {
		t.Fatalf("CaptureRunDataObservation = %+v (%q, %t)", direct.Header(), tenant, observed)
	}

	headerOnly, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	if tenant, observed := headerOnly.TenantID(); tenant != "" || observed {
		t.Fatalf("header-only binding carries (%q, %t), want (\"\", false)", tenant, observed)
	}
	legacy, err := WithRunDataBinding(t.Context(), headerOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{"team-a", ""} {
		if err := CheckRunObservedTenant(legacy, stored); err != nil {
			t.Fatalf("header-only binding, stored %q: %v, want the legacy admission", stored, err)
		}
		if err := CheckRunObservedTenant(t.Context(), stored); err != nil {
			t.Fatalf("unbound, stored %q: %v, want the legacy admission", stored, err)
		}
	}
	cancelled, cancel := context.WithCancel(teamA)
	cancel()
	if err := CheckRunObservedTenant(cancelled, "team-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// TestRunDataBindingMergeKeepsTheObservedTenant: a re-bind stores the merge of
// what ctx carries and what arrives, in either order. A header-only re-bind
// never drops an observed tenant; two observed tenants must agree; the header
// stays strictly compared.
func TestRunDataBindingMergeKeepsTheObservedTenant(t *testing.T) {
	t.Parallel()
	h := RunDataHeader{RunID: "r", Format: LegacyValueFormat, BirthID: "birth-a"}
	headerOnly, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	observed := func(tenant string) RunDataBinding {
		t.Helper()
		o, err := CaptureRunObservation(h, tenant, true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := CaptureRunDataObservation(o)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bind := func(ctx context.Context, bs ...RunDataBinding) (context.Context, error) {
		var err error
		for _, b := range bs {
			if ctx, err = WithRunDataBinding(ctx, b); err != nil {
				return nil, err
			}
		}
		return ctx, nil
	}

	// Header-only first, then the observation: the observed tenant is adopted.
	ctx, err := bind(t.Context(), headerOnly, observed("team-a"))
	if err != nil {
		t.Fatalf("header-only then observed: %v", err)
	}
	if tenant, ok := boundTenant(t, ctx); tenant != "team-a" || !ok {
		t.Fatalf("header-only then observed carries (%q, %t), want (\"team-a\", true)", tenant, ok)
	}

	// The observation first, then a header-only re-bind: kept, not dropped.
	ctx, err = bind(t.Context(), observed("team-a"), headerOnly)
	if err != nil {
		t.Fatalf("observed then header-only: %v", err)
	}
	if tenant, ok := boundTenant(t, ctx); tenant != "team-a" || !ok {
		t.Fatalf("observed then header-only carries (%q, %t), want (\"team-a\", true): the re-bind dropped the tenant", tenant, ok)
	}
	if err := CheckRunObservedTenant(ctx, "team-b"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("after a header-only re-bind, stored \"team-b\": %v, want ErrRunDataIdentityChanged", err)
	}
	if same, err := bind(ctx, observed("team-a")); err != nil {
		t.Fatalf("the same observation again: %v", err)
	} else if tenant, ok := boundTenant(t, same); tenant != "team-a" || !ok {
		t.Fatalf("the same observation again carries (%q, %t)", tenant, ok)
	}

	// Two observed tenants that differ, "" included, in both orders.
	for _, pair := range [][2]string{{"team-a", "team-b"}, {"team-a", ""}, {"", "team-a"}} {
		if _, err := bind(t.Context(), observed(pair[0]), observed(pair[1])); !errors.Is(err, ErrRunDataIdentityChanged) {
			t.Fatalf("observed %q then observed %q: %v, want ErrRunDataIdentityChanged", pair[0], pair[1], err)
		}
		if _, err := bind(t.Context(), observed(pair[0]), headerOnly, observed(pair[1])); !errors.Is(err, ErrRunDataIdentityChanged) {
			t.Fatalf("observed %q, header-only, then observed %q: %v, want ErrRunDataIdentityChanged", pair[0], pair[1], err)
		}
	}

	// The header stays strict whatever the tenant.
	successor := h
	successor.BirthID = "birth-b"
	successorOnly, err := CaptureRunDataBinding(successor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bind(t.Context(), observed("team-a"), successorOnly); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("observed then a successor header: %v, want ErrRunDataIdentityChanged", err)
	}

	// BindRunObservation goes through the same merge.
	viaBind := bindTenantObservation(t, t.Context(), h, "team-a")
	if viaBind, err = WithRunDataBinding(viaBind, headerOnly); err != nil {
		t.Fatal(err)
	}
	o, err := CaptureRunObservation(h, "team-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindRunObservation(viaBind, o); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("BindRunObservation of \"team-b\" over a carried \"team-a\": %v, want ErrRunDataIdentityChanged", err)
	}
}
