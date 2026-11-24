// Package testinventory reads Go's runnable test names without executing their
// bodies. Discovery still runs init and TestMain; it owns their process group.
package testinventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// Read discovers one package under the caller's build environment. On Unix it
// cancels the owned process group and cleans up remaining group members before
// returning. Descendants that create another session escape that group. Windows
// is refused with errors.ErrUnsupported because the shared process helper there
// kills only the direct child; the package-partition mode needs no discovery.
func Read(ctx context.Context, pkg string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("test discovery requires Unix process-group ownership: %w", errors.ErrUnsupported)
	}
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=vendor", "-json", "-list", "^(Test|Example|Fuzz)", pkg)
	proc.TerminateGroupOnCancel(cmd)
	proc.SetWaitDelay(cmd, time.Second)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start go test -list: %w", err)
	}
	// os/exec stops watching the context after reaping the direct child.
	// Keep ownership through the pipe drain too, including an operator's
	// ITERION_PROC_WAIT_DELAY=0. Join the callback before leaving teardown.
	cancelled := make(chan struct{})
	var cancelErr error
	stop := context.AfterFunc(ctx, func() {
		_, cancelErr = proc.KillProcessGroup(cmd)
		close(cancelled)
	})
	err := cmd.Wait()
	if !stop() {
		<-cancelled
	}
	_, cleanupErr := proc.KillProcessGroup(cmd)
	if err = errors.Join(err, cancelErr, cleanupErr, ctx.Err()); err != nil {
		return nil, fmt.Errorf("go test -list %s failed: %w: %s", pkg, err, strings.TrimSpace(stderr.String()))
	}
	return Parse(&stdout)
}

// Parse reads a complete, successful go test -json -list stream for one package.
// Output events are chunks, not lines: a name may span several of them.
func Parse(input io.Reader) ([]string, error) {
	decoder := json.NewDecoder(input)
	var pkg, pending string
	var started, passed bool
	seen := make(map[string]bool)
	var names []string
	for {
		var event struct{ Action, Package, ImportPath, Output string }
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("invalid go test -list JSON: %w", err)
		}
		// Go also emits compiler diagnostics as BuildEvents. They name an
		// ImportPath, not a tested Package, and their text is never a test
		// inventory. Only successful build output is admissible; build-fail
		// (or a future unknown build action) must not hide behind a later pass.
		if strings.HasPrefix(event.Action, "build-") {
			if event.Action != "build-output" || event.ImportPath == "" || event.Package != "" {
				return nil, fmt.Errorf("test discovery has invalid or failed build event %q for %q", event.Action, event.ImportPath)
			}
			continue
		}
		if event.Package == "" {
			return nil, fmt.Errorf("test discovery event has no package")
		}
		if pkg == "" {
			pkg = event.Package
		} else if event.Package != pkg {
			return nil, fmt.Errorf("-tests requires one package, found %s and %s", pkg, event.Package)
		}
		switch event.Action {
		case "start":
			if started {
				return nil, fmt.Errorf("test discovery started %s twice", pkg)
			}
			started = true
		case "pass":
			passed = true
		case "output":
			pending += event.Output
			for {
				end := strings.IndexByte(pending, '\n')
				if end < 0 {
					break
				}
				line := pending[:end]
				pending = pending[end+1:]
				if !token.IsIdentifier(line) || !strings.HasPrefix(line, "Test") && !strings.HasPrefix(line, "Example") && !strings.HasPrefix(line, "Fuzz") {
					continue
				}
				if seen[line] {
					return nil, fmt.Errorf("duplicate discovered test %s", line)
				}
				seen[line] = true
				names = append(names, line)
			}
		default:
			return nil, fmt.Errorf("test discovery for %s reported %q instead of a successful inventory", pkg, event.Action)
		}
	}
	if !started || !passed || len(names) == 0 || pending != "" {
		return nil, fmt.Errorf("test discovery did not complete a nonempty line-terminated inventory (package %q, started %v, passed %v, names %d, trailing bytes %d)", pkg, started, passed, len(names), len(pending))
	}
	sort.Strings(names)
	return names, nil
}
