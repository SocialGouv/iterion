package git

import (
	"slices"
	"testing"
)

// The config must precede the subcommand — `git fetch -c gc.auto=0` is a fetch
// flag error, not a config override — and one call must not be able to corrupt
// the next: a shared backing array would let the first caller's args leak into
// a second caller's command line.
func TestNoAutoMaintenance(t *testing.T) {
	got := NoAutoMaintenance("fetch", "--depth", "1")
	want := []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "--depth", "1"}
	if !slices.Equal(got, want) {
		t.Fatalf("NoAutoMaintenance = %q, want %q", got, want)
	}

	second := NoAutoMaintenance("checkout", "--force")
	if !slices.Equal(got, want) {
		t.Errorf("a second call rewrote the first call's args: %q", got)
	}
	if slices.Equal(second, got) {
		t.Errorf("both calls returned the same command line: %q", second)
	}

	if bare := NoAutoMaintenance(); !slices.Equal(bare, want[:4]) {
		t.Errorf("NoAutoMaintenance() = %q, want just the config %q", bare, want[:4])
	}
}
