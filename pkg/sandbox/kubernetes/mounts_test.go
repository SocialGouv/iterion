package kubernetes

import (
	"strings"
	"testing"
)

func TestParseMount_PVC(t *testing.T) {
	got, err := parseMount("type=pvc,source=cargo-cache,target=/cargo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Type != MountTypePVC {
		t.Errorf("Type = %q, want pvc", got.Type)
	}
	if got.Source != "cargo-cache" {
		t.Errorf("Source = %q", got.Source)
	}
	if got.Target != "/cargo" {
		t.Errorf("Target = %q", got.Target)
	}
	if got.ReadOnly {
		t.Errorf("ReadOnly should default to false")
	}
}

func TestParseMount_ConfigMapWithKeyAndReadOnly(t *testing.T) {
	got, err := parseMount("type=configmap,source=app-cfg,target=/etc/app/config.json,key=config.json,readonly")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Type != MountTypeConfigMap {
		t.Errorf("Type = %q", got.Type)
	}
	if got.Key != "config.json" {
		t.Errorf("Key = %q", got.Key)
	}
	if !got.ReadOnly {
		t.Error("ReadOnly should be true")
	}
}

func TestParseMount_RejectsRelativeTarget(t *testing.T) {
	_, err := parseMount("type=pvc,source=foo,target=cargo")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected absolute-path error, got: %v", err)
	}
}

func TestParseMount_RejectsUnknownKey(t *testing.T) {
	_, err := parseMount("type=pvc,source=foo,target=/bar,bogus=true")
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("expected unknown-key error, got: %v", err)
	}
}

func TestParseMount_RejectsMissingType(t *testing.T) {
	_, err := parseMount("source=foo,target=/bar")
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Errorf("expected type-required error, got: %v", err)
	}
}

func TestTranslateMounts_RejectsBind(t *testing.T) {
	_, _, err := translateMounts([]string{"type=bind,source=/host,target=/in"})
	if err == nil {
		t.Fatal("expected rejection of type=bind in cloud")
	}
	if !strings.Contains(err.Error(), "type=bind is not supported") {
		t.Errorf("error should explain bind rejection, got: %v", err)
	}
	if !strings.Contains(err.Error(), "type=pvc") {
		t.Errorf("error should suggest pvc alternative, got: %v", err)
	}
}

func TestTranslateMounts_PVCAndConfigMap(t *testing.T) {
	volumes, volumeMounts, err := translateMounts([]string{
		"type=pvc,source=cargo-cache,target=/cargo",
		"type=configmap,source=app-cfg,target=/etc/app.json,key=app.json,readonly",
		"type=secret,source=db-creds,target=/secrets",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(volumes) != 3 || len(volumeMounts) != 3 {
		t.Fatalf("len(volumes)=%d, len(volumeMounts)=%d, want 3 each", len(volumes), len(volumeMounts))
	}

	// Volume 0: PVC.
	pvc, ok := volumes[0]["persistentVolumeClaim"].(map[string]any)
	if !ok || pvc["claimName"] != "cargo-cache" {
		t.Errorf("volumes[0] = %v, want PVC claimName=cargo-cache", volumes[0])
	}
	if volumeMounts[0]["mountPath"] != "/cargo" {
		t.Errorf("volumeMounts[0].mountPath = %v, want /cargo", volumeMounts[0]["mountPath"])
	}

	// Volume 1: ConfigMap with key projection + readOnly.
	cm, ok := volumes[1]["configMap"].(map[string]any)
	if !ok || cm["name"] != "app-cfg" {
		t.Errorf("volumes[1] = %v, want ConfigMap name=app-cfg", volumes[1])
	}
	if volumeMounts[1]["readOnly"] != true {
		t.Errorf("volumeMounts[1].readOnly = %v, want true", volumeMounts[1]["readOnly"])
	}
	if volumeMounts[1]["subPath"] != "app.json" {
		t.Errorf("volumeMounts[1].subPath = %v, want app.json", volumeMounts[1]["subPath"])
	}

	// Volume 2: Secret.
	sec, ok := volumes[2]["secret"].(map[string]any)
	if !ok || sec["secretName"] != "db-creds" {
		t.Errorf("volumes[2] = %v, want Secret secretName=db-creds", volumes[2])
	}
	if mode, ok := sec["defaultMode"].(int); !ok || mode != 0o400 {
		t.Errorf("Secret defaultMode = %v (%T), want 0400", sec["defaultMode"], sec["defaultMode"])
	}
}

func TestSanitizeVolumeName(t *testing.T) {
	cases := map[string]string{
		"Cargo-Cache":            "cargo-cache",
		"app/config":             "app-config",
		"_underscore_":           "underscore",
		"with.dot":               "with.dot",
		strings.Repeat("a", 100): strings.Repeat("a", 50),
	}
	for in, want := range cases {
		if got := sanitizeVolumeName(in); got != want {
			t.Errorf("sanitizeVolumeName(%q) = %q, want %q", in, got, want)
		}
	}
}
