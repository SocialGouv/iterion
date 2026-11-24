package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A cloud deployment that publishes no legal document starts — the texts are
// the operator's to write — but says so at WARN (ADR-160); a local server,
// or a cloud one that publishes its documents, stays quiet.
func TestLoadLegalSurface_WarnsOnACloudDeploymentWithNoDocument(t *testing.T) {
	withDoc := t.TempDir()
	if err := os.WriteFile(filepath.Join(withDoc, "mentions-legales.md"), []byte("# Mentions légales\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		mode     iterconfig.Mode
		dir      string
		wantWarn bool
		wantDocs int
	}{
		{"cloud, nothing configured", iterconfig.ModeCloud, "", true, 0},
		{"cloud, empty dir", iterconfig.ModeCloud, t.TempDir(), true, 0},
		{"cloud, one document", iterconfig.ModeCloud, withDoc, false, 1},
		{"local, nothing configured", iterconfig.ModeLocal, "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			var cfg iterconfig.Config
			cfg.Mode = tc.mode
			cfg.Legal.DocumentsDir = tc.dir
			cfg.Legal.ContactEmail = " contact@example.org "
			surface, err := loadLegalSurface(cfg, iterlog.New(iterlog.LevelInfo, &buf))
			if err != nil {
				t.Fatalf("loadLegalSurface: %v", err)
			}
			if len(surface.Documents) != tc.wantDocs {
				t.Errorf("documents = %d, want %d", len(surface.Documents), tc.wantDocs)
			}
			if surface.ContactEmail != "contact@example.org" {
				t.Errorf("contact = %q, want the trimmed address", surface.ContactEmail)
			}
			warned := strings.Contains(buf.String(), "no legal document published")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log:\n%s", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestLoadLegalSurface_RefusesAnUnreadableDir(t *testing.T) {
	var cfg iterconfig.Config
	cfg.Mode = iterconfig.ModeCloud
	cfg.Legal.DocumentsDir = filepath.Join(t.TempDir(), "missing")
	if _, err := loadLegalSurface(cfg, iterlog.New(iterlog.LevelError, &bytes.Buffer{})); err == nil || !strings.Contains(err.Error(), "ITERION_LEGAL_DOCUMENTS_DIR") {
		t.Fatalf("loadLegalSurface on a missing dir = %v, want an ITERION_LEGAL_DOCUMENTS_DIR error", err)
	}
}
