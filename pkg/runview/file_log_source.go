package runview

import (
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
)

// startLogSource spawns a goroutine that tails the run's run.log (via
// the shared runstream tailer) and pushes any appended bytes into buf, the
// run's RunLogBuffer it was started for (registered via prepareRunLogNoFile
// in detached / ensure-source mode). The buffer fans out to live WS
// subscribers and tracks its own running offset, so the tailer's file offset
// is ignored here.
//
// The tailer writes to its own buffer only, never to whichever buffer holds
// the run ID when a chunk lands: the final drain after done closes can run
// once another buffer is registered under the ID, and its bytes are not that
// buffer's (a closed buffer drops them).
//
// Mirrors startEventSource but operates on opaque byte streams rather
// than line-delimited JSON.
func startLogSource(s *Service, runID string, buf *RunLogBuffer, done <-chan struct{}) {
	path := filepath.Join(s.storeDir, "runs", runID, "run.log")
	emit := func(_ int64, chunk []byte) {
		_, _ = buf.Write(chunk)
	}
	go runstream.TailLogFile(path, done, emit, s.logger)
}
