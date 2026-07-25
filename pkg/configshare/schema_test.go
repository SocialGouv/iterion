package configshare

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeriveGrant_CategoryExpansion(t *testing.T) {
	editable := []string{"categories.{category}.feeds", "categories.{category}.editorial"}
	visible := []string{"categories.{category}.digest_title"}
	allowed, vis, err := DeriveGrant(editable, visible, "a11y")
	if err != nil {
		t.Fatalf("DeriveGrant: %v", err)
	}
	wantAllowed := []string{"categories.a11y.feeds", "categories.a11y.editorial"}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Errorf("allowed = %v, want %v", allowed, wantAllowed)
	}
	// visible = union(allowed, visible-templates), editable first, deduped.
	wantVisible := []string{"categories.a11y.feeds", "categories.a11y.editorial", "categories.a11y.digest_title"}
	if !reflect.DeepEqual(vis, wantVisible) {
		t.Errorf("visible = %v, want %v", vis, wantVisible)
	}
}

func TestDeriveGrant_MissingCategoryIsValidationError(t *testing.T) {
	_, _, err := DeriveGrant([]string{"categories.{category}.feeds"}, nil, "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation for missing category, got %v", err)
	}
}

func TestDeriveGrant_RejectsUnsafeCategory(t *testing.T) {
	for _, bad := range []string{"a.b", "../etc", "a b", "a/b", "__proto__.x"} {
		if _, _, err := DeriveGrant([]string{"categories.{category}.feeds"}, nil, bad); !errors.Is(err, ErrValidation) {
			t.Errorf("category %q must be rejected as ErrValidation, got %v", bad, err)
		}
	}
}

func TestDeriveGrant_LiteralPathsPassThrough(t *testing.T) {
	// A bot with no {category} placeholder mints without a category.
	allowed, vis, err := DeriveGrant([]string{"settings.feeds"}, []string{"settings.title"}, "")
	if err != nil {
		t.Fatalf("DeriveGrant literal: %v", err)
	}
	if !reflect.DeepEqual(allowed, []string{"settings.feeds"}) {
		t.Errorf("allowed = %v", allowed)
	}
	if !reflect.DeepEqual(vis, []string{"settings.feeds", "settings.title"}) {
		t.Errorf("visible = %v", vis)
	}
}

func TestDeriveGrant_NoEditablePathsIsError(t *testing.T) {
	if _, _, err := DeriveGrant(nil, []string{"categories.{category}.digest_title"}, "a11y"); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation when no editable paths, got %v", err)
	}
}

func TestDeriveGrant_UnresolvedPlaceholderFailsClosed(t *testing.T) {
	// A typo'd placeholder must not pin a path with a literal "{…}" segment.
	if _, _, err := DeriveGrant([]string{"categories.{cat}.feeds"}, nil, "a11y"); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation for unresolved placeholder, got %v", err)
	}
}

func TestDeriveGrant_SubsetSelectsFields(t *testing.T) {
	editable := []string{"categories.{category}.feeds", "categories.{category}.editorial"}
	visible := []string{"categories.{category}.digest_title"}
	// Feeds-only share: editorial is neither writable NOR visible.
	allowed, vis, err := DeriveGrant(editable, visible, "a11y", "feeds")
	if err != nil {
		t.Fatalf("DeriveGrant subset: %v", err)
	}
	if !reflect.DeepEqual(allowed, []string{"categories.a11y.feeds"}) {
		t.Errorf("allowed = %v, want [categories.a11y.feeds]", allowed)
	}
	wantVisible := []string{"categories.a11y.feeds", "categories.a11y.digest_title"}
	if !reflect.DeepEqual(vis, wantVisible) {
		t.Errorf("visible = %v, want %v (editorial must NOT be visible)", vis, wantVisible)
	}
	for _, p := range vis {
		if p == "categories.a11y.editorial" {
			t.Errorf("editorial leaked into visible for a feeds-only share: %v", vis)
		}
	}
}

func TestDeriveGrant_SubsetRejectsUndeclaredField(t *testing.T) {
	editable := []string{"categories.{category}.feeds", "categories.{category}.editorial"}
	// `sinks` is not a declared editable field (it's hard-forbidden) — a share
	// can't select it.
	if _, _, err := DeriveGrant(editable, nil, "a11y", "sinks"); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation selecting an undeclared field, got %v", err)
	}
}

func TestDeriveGrant_DerivedPathsPassValidatePaths(t *testing.T) {
	// The whole point: the mint runs DeriveGrant output through ValidatePaths.
	allowed, vis, err := DeriveGrant(
		[]string{"categories.{category}.feeds", "categories.{category}.editorial"},
		[]string{"categories.{category}.digest_title"}, "cyber")
	if err != nil {
		t.Fatalf("DeriveGrant: %v", err)
	}
	if err := ValidatePaths(allowed, vis); err != nil {
		t.Fatalf("derived paths must satisfy ValidatePaths: %v", err)
	}
}
