package sandbox

import (
	"strings"
	"testing"
)

// The admission half of the read-back contract (ADR-143 §1): a declared
// read-back path is refused when no mount serves it, whatever the driver —
// the declaration is data, and a promise about a directory nobody has is
// the defect the bind-dropping rule exists for (#815).

func TestValidateReadBackPathWithoutAMountIsRefused(t *testing.T) {
	spec := &Spec{
		Mode:          ModeInline,
		Image:         "example.invalid/sbx:test",
		ReadBackPaths: []string{"/iterion/artifact-files"},
	}
	err := spec.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want a declared path no mount serves refused")
	}
	if !strings.Contains(err.Error(), "/iterion/artifact-files") {
		t.Errorf("error = %v, want it naming the declared path", err)
	}
}

func TestValidateReadBackPathServedByAMountIsAccepted(t *testing.T) {
	cases := []struct {
		name  string
		mount string
	}{
		{"the host bind", "source=/tmp/scratch,target=/iterion/artifact-files,type=bind"},
		{"driver-owned storage", "type=emptydir,target=/iterion/artifact-files,size=256Mi"},
		{"a parent target serves the path", "source=/tmp/scratch,target=/iterion,type=bind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &Spec{
				Mode:          ModeInline,
				Image:         "example.invalid/sbx:test",
				Mounts:        []string{tc.mount},
				ReadBackPaths: []string{"/iterion/artifact-files"},
			}
			if err := spec.Validate(); err != nil {
				t.Errorf("Validate = %v, want the declared path accepted under its serving mount", err)
			}
		})
	}
}

func TestValidateReadBackPathShape(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"relative", "iterion/artifact-files", "clean absolute"},
		{"not clean", "/iterion/./artifact-files", "clean absolute"},
		{"root", "/", "clean absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &Spec{
				Mode:          ModeInline,
				Image:         "example.invalid/sbx:test",
				Mounts:        []string{"type=emptydir,target=/iterion/artifact-files"},
				ReadBackPaths: []string{tc.path},
			}
			err := spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want %q refused with %q", err, tc.path, tc.want)
			}
		})
	}
}
