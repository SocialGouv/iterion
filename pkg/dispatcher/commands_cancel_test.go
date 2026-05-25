package dispatcher

import (
	"bytes"
	"os/exec"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestForceRemoveSandboxContainer_EmptyRunIDIsNoop ensures the helper
// is safe to call with an empty runID. Without this guard a defensive
// caller chain could accidentally invoke `docker rm --force iterion-`
// and try to remove a container whose name has no run-id suffix.
func TestForceRemoveSandboxContainer_EmptyRunIDIsNoop(t *testing.T) {
	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	// Returns without panic. No way to observe a no-op directly, so
	// the test passes by reaching the next line.
	forceRemoveSandboxContainer(logger, "")
}

// TestForceRemoveSandboxContainer_MissingContainerIsCleanlyHandled
// drives the helper against a container name that definitely does not
// exist (random uuid-like suffix). Docker reports "No such container",
// which the helper must swallow without error and without a noisy log
// line. Skipped in environments without `docker` on PATH so CI without
// a docker daemon doesn't break.
func TestForceRemoveSandboxContainer_MissingContainerIsCleanlyHandled(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH — skipping force-remove smoke test")
	}
	logsBuf := &bytes.Buffer{}
	logger := iterlog.New(iterlog.LevelDebug, logsBuf)
	// Use a runID that is overwhelmingly unlikely to collide with a
	// real container on the test host.
	const fakeRunID = "test-orphan-019eaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	forceRemoveSandboxContainer(logger, fakeRunID)
	// The helper must NOT emit at INFO level when the container is
	// missing — that is the happy path. (We don't check exact strings
	// because docker output wording varies between versions.)
	if bytes.Contains(logsBuf.Bytes(), []byte("force-removed sandbox container")) {
		t.Errorf("missing container should not log an INFO-level success line; got:\n%s", logsBuf.String())
	}
}
