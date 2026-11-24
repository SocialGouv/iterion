package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMetadataFormatFilesystemGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		replace string
		want    error
	}{
		{"known2", "2", nil}, {"future", "99", ErrUnsupportedValueFormat},
		{"null", "null", ErrInvalidValueFormat}, {"array", "[2]", ErrInvalidValueFormat},
		{"duplicate", "2,\"format_version\":99", ErrInvalidValueFormat},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: 2}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.runJSONPath("r"))
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(raw), `"format_version": 2`, `"format_version": `+tt.replace, 1)
			if changed == string(raw) && tt.name != "known2" {
				t.Fatal("fixture header absent")
			}
			if err = os.WriteFile(s.runJSONPath("r"), []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			called := 0
			sidecar := filepath.Join(s.runDir("r"), "metadata-probe")
			err = s.withRunMetadataMutation(t.Context(), "r", func(parent *Run) error {
				called++
				if parent == nil {
					t.Fatal("parent missing")
				}
				return os.WriteFile(sidecar, []byte("ok"), 0600)
			})
			if tt.want != nil {
				if !errors.Is(err, tt.want) || called != 0 {
					t.Fatal(err, called)
				}
				if _, err = os.Stat(sidecar); !os.IsNotExist(err) {
					t.Fatal("refusal wrote sidecar", err)
				}
			} else if err != nil || called != 1 {
				t.Fatal(err, called)
			}
		})
	}
}

func TestRunMetadataFormatFilesystemBindingAndTail(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: 2, Inputs: map[string]any{"n": int64(9007199254740993)}}); err != nil {
		t.Fatal(err)
	}
	err = s.withRunMetadataMutation(t.Context(), "r", func(parent *Run) error {
		parent.RunnerVersion = "changed"
		return s.writeRunLockedContext(t.Context(), parent, false, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadRun(t.Context(), "r")
	if err != nil || loaded.RunnerVersion != "changed" || loaded.Inputs["n"] != int64(9007199254740993) {
		t.Fatal(loaded, err)
	}
	bound, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: 1})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.withRunMetadataMutation(bound, "r", func(*Run) error { called = true; return nil })
	if !errors.Is(err, ErrValueFormatChanged) || called {
		t.Fatal(err, called)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.withRunMetadataMutation(cancelled, "r", func(*Run) error { t.Fatal("cancelled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = s.withRunMetadataMutation(t.Context(), "missing", func(parent *Run) error {
		if parent != nil {
			t.Fatal(parent)
		}
		return ErrRunNotFound
	}); !errors.Is(err, ErrRunNotFound) {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.runJSONPath("missing")); !os.IsNotExist(err) {
		t.Fatal("guard created parent", err)
	}
}
