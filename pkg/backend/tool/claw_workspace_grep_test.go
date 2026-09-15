package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWorkspaceGrepAliasesHonorCancellation(t *testing.T) {
	reg := NewRegistry()
	root := t.TempDir()
	if err := RegisterClawBuiltins(reg, root); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(reg, root, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, name := range []string{"grep", "workspace_grep"} {
		td, err := reg.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := td.Execute(ctx, json.RawMessage(`{"pattern":"needle"}`)); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s cancellation=%v", name, err)
		}
	}
}

func TestWorkspaceGrepBoundsSparseSearch(t *testing.T) {
	for _, tc := range []struct {
		name           string
		entries, input int
		reason         string
	}{
		{"entries", 7, 10000, "entry limit (7)"},
		{"input across files", 1000, 30, "input limit (30 bytes)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := range 40 {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%03d.txt", i)), []byte("boring\nboring\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := executeWorkspaceGrepWithLimits(t.Context(), map[string]any{"pattern": "needle"}, root, workspaceGrepLimits{maxResults: 100, maxOutputBytes: 512, maxVisitedEntries: tc.entries, maxScannedBytes: tc.input})
			if err != nil || !strings.Contains(out, tc.reason) || strings.Contains(out, "No matches") {
				t.Fatalf("sparse search=%q err=%v", out, err)
			}
		})
	}
}

func TestWorkspaceGrepInputCutPreservesOnlyCompleteLines(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("needle\nneedle-tail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := workspaceGrepLimits{maxResults: 100, maxOutputBytes: 512, maxScannedBytes: 13}
	out, err := executeWorkspaceGrepWithLimits(t.Context(), map[string]any{"pattern": "needle$"}, root, limits)
	if err != nil || !strings.Contains(out, "input.txt:1:needle\n") || strings.Contains(out, "input.txt:2:") || !strings.Contains(out, "input limit (13 bytes)") {
		t.Fatalf("cut line=%q err=%v", out, err)
	}
	limits.maxOutputBytes = 8
	if out, err := executeWorkspaceGrepWithLimits(t.Context(), map[string]any{"pattern": "absent"}, root, limits); err == nil || out != "" {
		t.Fatalf("tiny output silently hides truncation: %q %v", out, err)
	}
}

type grepTestReader struct {
	data         string
	terminal     error
	calls, bytes int
	cancel       context.CancelFunc
}

func (r *grepTestReader) Read(p []byte) (int, error) {
	r.calls++
	n := copy(p, r.data)
	r.data = r.data[n:]
	r.bytes += n
	if r.cancel != nil {
		r.cancel()
	}
	if len(r.data) == 0 && r.terminal != nil {
		return n, r.terminal
	}
	return n, nil
}

func TestWorkspaceGrepReaderBudgetAndEOF(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		budget     int
		terminal   error
		matches    int
		limited    bool
	}{
		{"unterminated true EOF", "needle", 6, io.EOF, 1, false},
		{"unterminated before EOF", "needle", 7, io.EOF, 1, false},
		{"unconfirmed exact boundary", "needle", 6, nil, 0, true},
		{"newline at boundary", "needle\n", 7, nil, 1, true},
		{"newline and cut tail", "needle\nneedle-tail", 13, nil, 1, true},
		{"EOF on final newline", "needle\n", 7, io.EOF, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining := tc.budget
			source := &grepTestReader{data: tc.data, terminal: tc.terminal}
			r := &grepBudgetReader{ctx: t.Context(), source: source, remaining: &remaining}
			matches, _, _, err := grepWorkspaceReader(r, regexp.MustCompile("needle$"), "input", 100, 1000, false, "results", "output")
			if len(matches) != tc.matches || errors.Is(err, errGrepInputLimit) != tc.limited || (err != nil && !tc.limited) {
				t.Fatalf("matches=%v err=%v", matches, err)
			}
			if source.calls != 1 || source.bytes > tc.budget || remaining != tc.budget-source.bytes {
				t.Fatalf("unaccounted reads: %+v remaining=%d", source, remaining)
			}
		})
	}
	// Data accompanying an ordinary error still consumes the shared allowance.
	remaining := 8
	source := &grepTestReader{data: "bytes", terminal: io.ErrUnexpectedEOF}
	r := &grepBudgetReader{ctx: t.Context(), source: source, remaining: &remaining}
	if n, err := r.Read(make([]byte, 64)); n != 5 || !errors.Is(err, io.ErrUnexpectedEOF) || remaining != 3 {
		t.Fatalf("error read=%d %v remaining=%d", n, err, remaining)
	}
}

func TestWorkspaceGrepCancellationStopsBufferedScanAndFurtherReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &grepTestReader{data: strings.Repeat("needle\n", 100), cancel: cancel}
	remaining := 1000
	r := &grepBudgetReader{ctx: ctx, source: source, remaining: &remaining}
	matches, _, _, err := grepWorkspaceReader(r, regexp.MustCompile("needle"), "input", 100, 10000, false, "results", "output")
	if !errors.Is(err, context.Canceled) || len(matches) != 0 {
		t.Fatalf("cancelled scan=%v %v", matches, err)
	}
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, context.Canceled) || source.calls != 1 {
		t.Fatalf("read after cancellation: calls=%d err=%v", source.calls, err)
	}
}

func TestWorkspaceGrepTraversalKeepsExclusionsAndOrder(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, path := range []string{"z.txt", "a.txt", "nested/b.txt", ".git/hidden", "node_modules/hidden", ".ssh/hidden", ".env/hidden"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("needle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"loop": root, "escape": outside} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := executeWorkspaceGrep(t.Context(), map[string]any{"pattern": "needle"}, root)
	want := strings.Join([]string{filepath.Join(root, "a.txt") + ":1:needle", filepath.Join(root, "nested/b.txt") + ":1:needle", filepath.Join(root, "z.txt") + ":1:needle"}, "\n")
	if err != nil || out != want {
		t.Fatalf("traversal=%q err=%v, want %q", out, err, want)
	}
	// The traversal itself stops between entries without depending on timing.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	visits := 0
	_, err = walkWorkspaceGrep(ctx, root, info, 100, func(string, os.FileInfo) error {
		visits++
		if visits == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 2 {
		t.Fatalf("cancelled traversal visits=%d err=%v", visits, err)
	}
}
