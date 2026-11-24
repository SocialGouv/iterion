package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// L5-1: the on-demand tailer labels its buffer with the run it tails — the
// one holding the ID now, read without the subscriber's binding. A
// subscription admitted on a predecessor just before an active successor
// replaced it stands the tailer up, yet is not served the successor's live
// log under the predecessor's label; a subscription admitted on the
// successor is, and a late one admitted on the predecessor leaves it alone.
func TestTailerBufferLabelledFromTheRunItTails(t *testing.T) {
	svc, native, _ := newOriginStreamService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	boundA := boundOriginCtx(t, svc, "r")
	admittedA, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	successor, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.storeDir, "runs", "r", "run.log"), []byte("SUCCESSOR live bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	planA := svc.planLogs(boundA, "r", admittedA)
	if planA.release != nil {
		defer planA.release()
	}
	if planA.logSub != nil {
		defer planA.logSub.Cancel()
	}
	if planA.buf != nil {
		t.Fatalf("a subscription admitted on the predecessor is served a live buffer (born of the predecessor: %v, of the successor: %v)", planA.buf.BornOf(admittedA.BirthID), planA.buf.BornOf(successor.BirthID))
	}

	planB := svc.planLogs(boundOriginCtx(t, svc, "r"), "r", successor)
	if planB.release != nil {
		defer planB.release()
	}
	if planB.logSub != nil {
		defer planB.logSub.Cancel()
	}
	if planB.buf == nil || !planB.buf.BornOf(successor.BirthID) {
		t.Fatal("a subscription admitted on the successor is not served the successor's live buffer")
	}

	late := svc.planLogs(boundA, "r", admittedA)
	if late.release != nil {
		defer late.release()
	}
	if late.buf != nil || svc.GetLogBuffer("r") != planB.buf {
		t.Fatal("a subscription admitted on the predecessor disturbed the current run's live buffer")
	}
}

// R6: a tailer writes to the buffer it was started for, never to whichever
// buffer holds the run ID when a chunk lands. Another buffer is registered
// under the ID before the tailer's final drain (done closed after the swap):
// the drained line lands in the tailer's own buffer, the other buffer gets
// none of it.
func TestTailerFinalDrainWritesOnlyItsOwnBuffer(t *testing.T) {
	svc, native, _ := newOriginStreamService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(svc.storeDir, "runs", "r", "run.log")
	if err := os.WriteFile(logPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := svc.prepareRunLogNoFile(ctx, "r")
	done := make(chan struct{})
	startLogSource(svc, "r", own, done)
	if got := waitBufferHolds(own, "first\n"); !strings.Contains(got, "first\n") {
		t.Fatalf("the tailer never fed its buffer: %q", got)
	}

	other, _ := NewRunLogBuffer("")
	svc.runLogsMu.Lock()
	svc.runLogs["r"] = other
	svc.runLogsMu.Unlock()
	t.Cleanup(func() { svc.dropRunLog("r", other) })
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("second\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	close(done)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(bufferText(own), "second\n") && bufferText(other) == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if got := bufferText(other); got != "" {
		t.Fatalf("the tailer wrote %q into the buffer registered under the ID since", got)
	}
	if got := bufferText(own); !strings.Contains(got, "second\n") {
		t.Fatalf("the tailer's final drain never reached its own buffer: %q", got)
	}
}

// R7-1: the last release of a tailer drops its own buffer only. A new
// subscription stands a tailer up between the release unlocking and
// dropping (the barrier): its buffer stays registered and live.
func TestLogSourceReleaseLeavesTheBufferInstalledSince(t *testing.T) {
	svc, native, _ := newOriginStreamService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	release, old := svc.ensureLogSource(ctx, "r")
	var next func()
	var current *RunLogBuffer
	svc.logReleaseGap = func(runID string) {
		next, current = svc.ensureLogSource(ctx, runID)
	}
	release()
	svc.logReleaseGap = nil
	if next == nil || current == nil || current == old {
		t.Fatal("fixture: no new tailer stood up in the gap")
	}
	defer next()
	if got := svc.GetLogBuffer("r"); got != current {
		t.Fatalf("the release dropped the buffer installed after it (registered %p, installed %p)", got, current)
	}
	sub := current.Subscribe()
	defer sub.Cancel()
	_, _ = current.Write([]byte("live\n"))
	select {
	case c, ok := <-sub.C:
		if !ok || string(c.Bytes) != "live\n" {
			t.Fatalf("the buffer installed after the release is closed (chunk %q, open %v)", c.Bytes, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the buffer installed after the release delivers nothing")
	}
}

// R6: a subscription to a closed buffer is closed already — no Close is left
// to end it.
func TestSubscribeToClosedLogBufferIsClosed(t *testing.T) {
	buf, _ := NewRunLogBuffer("")
	buf.Close()
	sub := buf.Subscribe()
	defer sub.Cancel()
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("a closed buffer delivered a chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a subscription to a closed buffer never ends")
	}
}

func bufferText(b *RunLogBuffer) string {
	_, data, _ := b.Snapshot(0)
	return string(data)
}

// waitBufferHolds waits up to 5s for b to hold want, and returns what it holds.
func waitBufferHolds(b *RunLogBuffer, want string) string {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(bufferText(b), want) {
		time.Sleep(10 * time.Millisecond)
	}
	return bufferText(b)
}
