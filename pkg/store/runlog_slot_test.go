package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TeeRunLog is where two launch surfaces (the CLI and the in-process
// dispatcher runner) build their per-run logger, and the handle it returns
// is the one runview.BuildExecutor installs the RUN's secret matcher on.
// The slot is per-handle, so the handle must be the run's own on BOTH
// paths: the tee, and the failure path that keeps the run on stderr only.
// Returning the caller's logger there would install one run's matcher
// where the next run replaces it (iterion#2822).
func TestTeeRunLogReturnsTheRunsOwnHandleOnBothPaths(t *testing.T) {
	const value = "hunter2-9f8e7d6c5b4a"
	redact := func(s string) string { return strings.ReplaceAll(s, value, "__RUN__") }

	cases := []struct {
		name       string
		runID      string
		root       func(t *testing.T) string
		wantCloser bool
	}{
		{
			// The failure path: an unsafe run ID is refused before any
			// path is built, so no tee exists and the run stays on stderr.
			name:  "no tee",
			runID: "../escape",
			root:  func(t *testing.T) string { return t.TempDir() },
		},
		{
			name:       "tee",
			runID:      "run-slot",
			root:       func(t *testing.T) string { return t.TempDir() },
			wantCloser: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var procBuf bytes.Buffer
			process := iterlog.New(iterlog.LevelInfo, &procBuf)
			root := tc.root(t)

			runLogger, closer := TeeRunLog(process, iterlog.LevelInfo, root, tc.runID)
			if closer != nil {
				defer closer.Close()
			}
			if (closer != nil) != tc.wantCloser {
				t.Fatalf("closer non-nil = %v, want %v", closer != nil, tc.wantCloser)
			}
			if runLogger == process {
				t.Fatal("TeeRunLog handed back the caller's own logger — this run's matcher would land on the logger every run shares")
			}

			runLogger.SetRedactor(redact)
			runLogger.Info("snapshot: %s", value)
			process.Info("process line: %s", value)

			// The process logger's OWN line never crossed the run's matcher
			// — its slot stayed empty, which is what keeps the next run's
			// lines out of this run's pass.
			if got := procBuf.String(); !strings.Contains(got, "process line: "+value) {
				t.Errorf("the run's matcher reached the process logger, which every run shares: %q", got)
			}
			if !tc.wantCloser {
				// No tee: the run's own line rides the same stderr, and it
				// did cross its matcher.
				if got := procBuf.String(); !strings.Contains(got, "snapshot: __RUN__") || strings.Contains(got, "snapshot: "+value) {
					t.Errorf("the run's line did not cross its matcher: %q", got)
				}
				return
			}
			logPath := filepath.Join(root, "runs", tc.runID, "run.log")
			b, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read run.log: %v", err)
			}
			if strings.Contains(string(b), value) || !strings.Contains(string(b), "__RUN__") {
				t.Errorf("run.log did not cross the run's matcher: %q", string(b))
			}
		})
	}
}
