package store

import (
	"context"
	"strings"
	"testing"
)

func TestOpen_LocalDispatch(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), OpenConfig{
		Mode:     "local",
		StoreDir: dir,
	})
	if err != nil {
		t.Fatalf("Open(local): %v", err)
	}
	if s == nil {
		t.Fatal("Open returned nil store")
	}
	caps := s.Capabilities()
	if !caps.PIDFile || !caps.GitWorktree {
		t.Errorf("expected filesystem caps PIDFile=true GitWorktree=true, got %+v", caps)
	}
}

func TestOpen_DefaultModeIsLocal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), OpenConfig{StoreDir: dir})
	if err != nil {
		t.Fatalf("Open(empty mode): %v", err)
	}
	if s == nil {
		t.Fatal("Open returned nil store")
	}
}

func TestOpen_LocalRequiresStoreDir(t *testing.T) {
	_, err := Open(context.Background(), OpenConfig{Mode: "local"})
	if err == nil {
		t.Fatal("expected error when StoreDir is empty in local mode")
	}
	if !strings.Contains(err.Error(), "StoreDir") {
		t.Errorf("error should mention StoreDir: %v", err)
	}
}

func TestOpen_UnknownModeFails(t *testing.T) {
	_, err := Open(context.Background(), OpenConfig{Mode: "not-a-mode"})
	if err == nil {
		t.Fatal("expected error on unknown mode")
	}
	if !strings.Contains(err.Error(), "not-a-mode") {
		t.Errorf("error should mention bad mode value: %v", err)
	}
	if strings.Contains(err.Error(), "ITERION_MODE") {
		t.Errorf("error should not leak the env-var name (config layer owns that): %v", err)
	}
}

func TestOpen_CloudNotYetBuilt(t *testing.T) {
	// The cloud branch returns an explicit error until plan §F T-19
	// wires Mongo+S3. Until then, callers should observe the gap.
	_, err := Open(context.Background(), OpenConfig{Mode: "cloud"})
	if err == nil {
		t.Fatal("expected explicit error from unimplemented cloud branch")
	}
}
