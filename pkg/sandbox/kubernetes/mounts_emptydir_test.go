package kubernetes

import (
	"strings"
	"testing"
)

// The emptydir mount type: how a declared read-back path is served on a
// pod (ADR-143 §2) — storage the driver owns, sized to the read-back cap.
// The parse and the render are pinned separately: the parse is what an
// author writes, the render is what the pod runs.

func TestParseMount_EmptyDir(t *testing.T) {
	got, err := parseMount("type=emptydir,target=/iterion/artifact-files,size=256Mi")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Type != MountTypeEmptyDir {
		t.Errorf("Type = %q, want emptydir", got.Type)
	}
	if got.Source != "" {
		t.Errorf("Source = %q, want none — driver-owned storage names no cluster object", got.Source)
	}
	if got.Target != "/iterion/artifact-files" {
		t.Errorf("Target = %q", got.Target)
	}
	if got.Size != "256Mi" {
		t.Errorf("Size = %q, want 256Mi", got.Size)
	}
}

func TestParseMount_EmptyDirRefusals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"a source is not a hint", "type=emptydir,source=pvc-name,target=/scratch", "takes no source"},
		{"a key is not a projection", "type=emptydir,target=/scratch,key=cfg", "takes no key"},
		{"target is still required", "type=emptydir", "target is required"},
		{"target stays absolute", "type=emptydir,target=relative", "must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMount(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseMount(%q) = %v, want it refused with %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestParseMount_SizeIsAnEmptydirKey(t *testing.T) {
	if _, err := parseMount("type=pvc,source=v,target=/v,size=1Gi"); err == nil || !strings.Contains(err.Error(), "emptydir key") {
		t.Errorf("parseMount = %v, want size= refused off an emptydir", err)
	}
}

func TestTranslateMounts_RendersAnEmptyDirVolume(t *testing.T) {
	volumes, mounts, err := translateMounts([]string{"type=emptydir,target=/iterion/artifact-files,size=256Mi"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(volumes) != 1 || len(mounts) != 1 {
		t.Fatalf("rendered %d volumes / %d mounts, want one of each", len(volumes), len(mounts))
	}
	// The volume is driver-owned storage at the declared path, capped.
	if _, ok := volumes[0]["emptyDir"]; !ok {
		t.Errorf("volume = %v, want an emptyDir", volumes[0])
	}
	volName, _ := volumes[0]["name"].(string)
	if volName == "" || strings.HasSuffix(volName, "-") {
		t.Errorf("volume name = %q, want a valid DNS-1123 label", volName)
	}
	if limit, ok := volumes[0]["emptyDir"].(map[string]any)["sizeLimit"]; !ok || limit != "256Mi" {
		t.Errorf("sizeLimit = %v, want 256Mi — the cap is carried onto the pod, not only into the drain", volumes[0]["emptyDir"])
	}
	if mounts[0]["mountPath"] != "/iterion/artifact-files" {
		t.Errorf("mountPath = %v, want the declared container path", mounts[0]["mountPath"])
	}
	if got, _ := mounts[0]["name"].(string); got != volName {
		t.Errorf("mount references %q, want its own volume %q", got, volName)
	}
}
