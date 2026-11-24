package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

// Process configuration is shared state: these tests deliberately do not use
// t.Parallel. Capture is otherwise a per-driver value, not a package cache.
func isolateTransportConfig(t *testing.T) {
	t.Helper()
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	for _, suffix := range []string{"HOST_HELPER", "GUEST_HELPER", "PROTOCOL", "DEFAULT_PATH", "SHELL", "START_TIMEOUT"} {
		key := execTransportPrefix + suffix
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(execTransportPrefix+"HOST_HELPER", "/operator/iterion")
}

func TestExecTransportConfigurationDefaultsAndPresence(t *testing.T) {
	isolateTransportConfig(t)
	cfg, err := loadExecTransportConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.hostHelper != "/operator/iterion" || cfg.guestHelper != defaultGuestHelper || cfg.profile.DefaultPath != "/bin:/usr/bin" || cfg.profile.Shell != "/bin/sh" || cfg.startTimeout != 30*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	t.Setenv(execTransportPrefix+"DEFAULT_PATH", "")
	t.Setenv(execTransportPrefix+"SHELL", "/operator/\xff-shell")
	t.Setenv(execTransportPrefix+"START_TIMEOUT", "0")
	cfg, err = loadExecTransportConfig()
	if err != nil || cfg.profile.DefaultPath != "" || cfg.profile.Shell != "/operator/\xff-shell" || cfg.startTimeout != 0 {
		t.Fatalf("operator presence lost: %+v %v", cfg, err)
	}
	t.Setenv(execTransportPrefix+"DEFAULT_PATH", "local::/bytes/\xff:../bin")
	cfg, err = loadExecTransportConfig()
	if err != nil || cfg.profile.DefaultPath != "local::/bytes/\xff:../bin" {
		t.Fatalf("native path changed: %+v %v", cfg, err)
	}
}

func TestExecTransportConfigurationRejectsUnsupportedChoices(t *testing.T) {
	cases := []struct{ suffix, value string }{
		{"HOST_HELPER", "relative"}, {"HOST_HELPER", ""},
		{"GUEST_HELPER", "relative"}, {"GUEST_HELPER", ""},
		{"PROTOCOL", "1"}, {"PROTOCOL", "99"}, {"PROTOCOL", ""},
		{"SHELL", "relative"}, {"SHELL", ""},
		{"START_TIMEOUT", "-1s"}, {"START_TIMEOUT", "invalid-secret"}, {"START_TIMEOUT", ""},
	}
	for _, tc := range cases {
		t.Run(tc.suffix+"/"+tc.value, func(t *testing.T) {
			isolateTransportConfig(t)
			t.Setenv(execTransportPrefix+tc.suffix, tc.value)
			_, err := loadExecTransportConfig()
			if !errors.Is(err, protocol.ErrTransport) {
				t.Fatalf("configuration accepted or untyped: %v", err)
			}
			if strings.Contains(err.Error(), "invalid-secret") {
				t.Fatalf("raw value disclosed: %v", err)
			}
		})
	}
}

func TestExecTransportConfigurationRefusesEveryPlantedComponent(t *testing.T) {
	for _, suffix := range []string{"HOST_HELPER", "GUEST_HELPER", "PROTOCOL", "DEFAULT_PATH", "SHELL", "START_TIMEOUT"} {
		t.Run(suffix, func(t *testing.T) {
			isolateTransportConfig(t)
			envtrust.MarkPlanted(execTransportPrefix + suffix)
			_, err := loadExecTransportConfig()
			var typed *protocol.Error
			if !errors.As(err, &typed) || typed.Kind != "operator-provenance" {
				t.Fatalf("planted control accepted: %v", err)
			}
		})
	}
}

func TestExecTransportConfigurationCapturesResolvedHostOnce(t *testing.T) {
	isolateTransportConfig(t)
	if err := os.Unsetenv(execTransportPrefix + "HOST_HELPER"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "iterion")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg, err := loadExecTransportConfig()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/unrelated")
	t.Setenv(execTransportPrefix+"SHELL", "/changed")
	if cfg.hostHelper != helper || cfg.profile.Shell != "/bin/sh" {
		t.Fatalf("capture changed: %+v", cfg)
	}
	envtrust.MarkPlanted("PATH")
	_, err = loadExecTransportConfig()
	if !errors.Is(err, protocol.ErrTransport) {
		t.Fatalf("planted lookup accepted: %v", err)
	}
}
