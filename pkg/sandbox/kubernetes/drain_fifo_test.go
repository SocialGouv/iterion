//go:build unix

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestExportRunFilesBoundsWhatItSaysOfDeclinedMembers: an archive is the
// pod's to fill, and every member the host declined cost a line in the
// run's log — teed into the store in cloud — and an entry held in memory,
// so a tool that left a hundred thousand fifos in its run-files dir became
// a hundred thousand records. The loss counts every one and names the first
// sandbox.ReadBackDeclinedListed in name order; the log warns as many and
// says in one line how many more it held back.
func TestExportRunFilesBoundsWhatItSaysOfDeclinedMembers(t *testing.T) {
	const fifos = sandbox.ReadBackDeclinedListed + 150
	pod := podTree(t, map[string]string{"report.md": "# audit"})
	for i := 1; i <= fifos; i++ {
		if err := syscall.Mkfifo(filepath.Join(pod, fmt.Sprintf("fifo-%04d", i)), 0o644); err != nil {
			t.Skipf("no fifo on this filesystem: %v", err)
		}
	}
	hostDst := t.TempDir()
	kubectl, _ := exportShim(t, pod, 0, "")
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: hostDst}}, kubectl)
	var runLog bytes.Buffer
	r.driver.logger = iterlog.New(iterlog.LevelWarn, &runLog)

	err := r.ExportRunFiles(context.Background())
	if got := sandbox.ReadBackLossReasonOf(err); got != sandbox.ReadBackDeclined {
		t.Fatalf("ExportRunFiles = %v (reason %q), want %q", err, got, sandbox.ReadBackDeclined)
	}
	if got := sandbox.ReadBackDeclinedCountOf(err); got != fifos {
		t.Errorf("declined count = %d, want every fifo counted (%d)", got, fifos)
	}
	named := sandbox.ReadBackDeclinedOf(err)
	if len(named) != sandbox.ReadBackDeclinedListed {
		t.Fatalf("the loss names %d members, want the first %d — the rest is a count, not an entry each", len(named), sandbox.ReadBackDeclinedListed)
	}
	if first, last := named[0].Name, named[len(named)-1].Name; first != "fifo-0001" || last != fmt.Sprintf("fifo-%04d", sandbox.ReadBackDeclinedListed) {
		t.Errorf("the loss names %s..%s, want the first ones in name order", first, last)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d member(s)", fifos)) {
		t.Errorf("error = %v, want the count of every declined member", err)
	}
	if lines := strings.Count(runLog.String(), "drain skipped"); lines != sandbox.ReadBackDeclinedListed {
		t.Errorf("the run's log warned %d declined members, want the first %d — every line past them is a record in the store", lines, sandbox.ReadBackDeclinedListed)
	}
	if want := fmt.Sprintf("%d more per-member line(s)", fifos-sandbox.ReadBackDeclinedListed); !strings.Contains(runLog.String(), want) {
		t.Errorf("the run's log never says what it held back (want %q)", want)
	}
	if got, readErr := os.ReadFile(filepath.Join(hostDst, "report.md")); readErr != nil || string(got) != "# audit" {
		t.Errorf("report.md = %q (%v), want the rest of the set landed past the declined members", got, readErr)
	}
}
