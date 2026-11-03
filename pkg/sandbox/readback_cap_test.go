package sandbox

import (
	"fmt"
	"strings"
	"testing"
)

// The read-back cap's escape hatches (ADR-143, philosophy #1: a bound with
// no override is a defect): the serving mount's own size, the deployment's
// env override, the default — in that order — and a figure that reads the
// Kubernetes grammar a bot writes on its mount.

func TestParseByteQuantity(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"256Mi", 256 << 20},
		{"2Gi", 2 << 30},
		{"1Ki", 1024},
		{"1Ti", 1 << 40},
		{"500M", 500_000_000},
		{"1k", 1000},
		{"1G", 1_000_000_000},
		{"1073741824", 1 << 30},
		{"1.5Gi", 3 << 29},
		{"1e9", 1_000_000_000},
		{"1E", 1_000_000_000_000_000_000},
		{"1Ei", 1 << 60},
		{"2.5", 3}, // whole bytes, rounded up
		{"1.5e1", 15},
		{" 64Mi ", 64 << 20},
		{".5Ki", 512},
		{"4.", 4},
	}
	for _, tc := range cases {
		got, err := ParseByteQuantity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseByteQuantity(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	refused := []string{"", "Mi", "abc", "-1Gi", "+1Gi", "0", "0Gi", "256m", "1n", "1u", "1x", "1e", "1e3Ki", "1/2", "0x10", "1.2.3", "9223372036854775808", "8Ei"}
	for _, in := range refused {
		if got, err := ParseByteQuantity(in); err == nil {
			t.Errorf("ParseByteQuantity(%q) = %d, want it refused", in, got)
		}
	}
}

// TestParseByteQuantityBoundsWhatABotCanMakeItCompute: the figure is read
// from a bot's own `size=` at every run start, and the parse scales by
// 10^e — an exponent left unbounded (`1e999999999`) holds the runner on a
// number hundreds of megabytes long. Each bound is refused for its own
// reason: without it, the near cases below fail later and otherwise
// (past int64, or accepted as one byte), and the far ones never return.
func TestParseByteQuantityBoundsWhatABotCanMakeItCompute(t *testing.T) {
	for _, tc := range []struct{ in, reason string }{
		{"1e65", "exponent 65"},
		{"1e-65", "exponent -65"},
		{"1e999999999", "exponent 999999999"},
		{"1e-999999999", "exponent -999999999"},
		{strings.Repeat("9", maxByteQuantityText+1), "longer than a byte quantity"},
	} {
		got, err := ParseByteQuantity(tc.in)
		if err == nil {
			t.Errorf("ParseByteQuantity(%.20q) = %d, want it refused", tc.in, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("ParseByteQuantity(%.20q) error = %v, want the bound named (%q)", tc.in, err, tc.reason)
		}
	}
	// At the bounds the reader still reads: the largest exponent (45 zeros
	// scale it back to 10^18 bytes) and the longest text.
	if got, err := ParseByteQuantity("0." + strings.Repeat("0", 45) + "1e64"); err != nil || got != 1_000_000_000_000_000_000 {
		t.Errorf("an exponent at the bound = %d, %v; want 10^18", got, err)
	}
	if got, err := ParseByteQuantity(strings.Repeat("0", maxByteQuantityText-3) + "1Gi"); err != nil || got != 1<<30 {
		t.Errorf("a %d-character value = %d, %v; want 1 GiB", maxByteQuantityText, got, err)
	}
}

func TestMountSizeOf(t *testing.T) {
	cases := map[string]string{
		"type=emptydir,target=/iterion/artifact-files,size=2Gi":  "2Gi",
		"type=emptydir, target=/x , size = 1Gi ":                 "1Gi",
		"type=emptydir,target=/iterion/artifact-files":           "",
		"type=pvc,source=persist,target=/iterion/artifact-files": "",
		"": "",
	}
	for in, want := range cases {
		if got := MountSizeOf(in); got != want {
			t.Errorf("MountSizeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadBackCapForWalksTheLadder(t *testing.T) {
	warns := func() (func(string, ...any), *[]string) {
		var got []string
		return func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, &got
	}
	t.Run("the default when nothing names a figure", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "")
		warn, got := warns()
		if cap := ReadBackCapFor("type=emptydir,target=/iterion/artifact-files", warn); cap != ReadBackCapBytes {
			t.Errorf("cap = %d, want the %d default", cap, ReadBackCapBytes)
		}
		if len(*got) != 0 {
			t.Errorf("warned %q on a figure nobody named", *got)
		}
	})
	t.Run("the serving mount's own size wins", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "1Gi")
		warn, _ := warns()
		if cap := ReadBackCapFor("type=emptydir,target=/iterion/artifact-files,size=2Gi", warn); cap != 2<<30 {
			t.Errorf("cap = %d, want the mount's own 2 GiB — the author's explicit figure, over the deployment's and the default", cap)
		}
	})
	t.Run("the env override stands in for a mount naming no size", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "1Gi")
		warn, _ := warns()
		if cap := ReadBackCapFor("type=pvc,source=persist,target=/iterion/artifact-files", warn); cap != 1<<30 {
			t.Errorf("cap = %d, want the 1 GiB override", cap)
		}
		if cap := ReadBackCapFor("", warn); cap != 1<<30 {
			t.Errorf("cap = %d with no serving mount, want the 1 GiB override", cap)
		}
	})
	t.Run("an unreadable size is reported and the next rung applies", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "512Mi")
		warn, got := warns()
		if cap := ReadBackCapFor("type=emptydir,target=/x,size=lots", warn); cap != 512<<20 {
			t.Errorf("cap = %d, want the env rung under an unreadable mount size", cap)
		}
		if len(*got) != 1 || !strings.Contains((*got)[0], `size="lots"`) || !strings.Contains((*got)[0], ReadBackCapEnv) {
			t.Errorf("warns = %q, want the unreadable size reported with the override named", *got)
		}
	})
	t.Run("an unreadable override is reported and the default kept", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "256m")
		warn, got := warns()
		if cap := ReadBackCapFor("", warn); cap != ReadBackCapBytes {
			t.Errorf("cap = %d, want the default kept under an unreadable override", cap)
		}
		if len(*got) != 1 || !strings.Contains((*got)[0], ReadBackCapEnv+`="256m"`) {
			t.Errorf("warns = %q, want the variable and its value reported", *got)
		}
	})
	t.Run("a nil warn is accepted", func(t *testing.T) {
		t.Setenv(ReadBackCapEnv, "bad")
		if cap := ReadBackCapFor("type=emptydir,target=/x,size=bad", nil); cap != ReadBackCapBytes {
			t.Errorf("cap = %d, want the default", cap)
		}
	})
}

// TestMountServingAnswersTheNearestMount: when mounts nest, the files at
// the path live in the deepest one's volume — it shadows its parent's
// under its own target — so its size is the figure that bounds them. The
// first serving mount in the list is the parent here: answering it would
// meter a bot's 2 GiB run-files volume on the 1 GiB of the tree above it.
func TestMountServingAnswersTheNearestMount(t *testing.T) {
	parent := "type=emptydir,target=/iterion,size=1Gi"
	child := "type=emptydir,target=/iterion/artifact-files,size=2Gi"
	if got := MountServing([]string{parent, child}, "/iterion/artifact-files"); got != child {
		t.Errorf("MountServing = %q, want the nearest mount %q", got, child)
	}
	if got := MountServing([]string{parent, child}, "/iterion/other"); got != parent {
		t.Errorf("MountServing = %q for a path only the parent serves, want %q", got, parent)
	}
	if got := ReadBackCapFor(MountServing([]string{parent, child}, "/iterion/artifact-files/out"), nil); got != 2<<30 {
		t.Errorf("cap = %d under the nested mount, want its own 2 GiB", got)
	}
}

func TestReadBackCapText(t *testing.T) {
	if got := ReadBackCapText(256 << 20); got != "256 MiB" {
		t.Errorf("ReadBackCapText(256Mi) = %q", got)
	}
	if got := ReadBackCapText(4096); got != "4096 bytes" {
		t.Errorf("ReadBackCapText(4096) = %q, want bytes, not 0 MiB", got)
	}
}
