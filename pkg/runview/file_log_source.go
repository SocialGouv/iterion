package runview

import (
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
)

// startLogSource spawns a goroutine that tails the run's run.log (via
// the shared runstream tailer) and pushes any appended bytes into the
// run's RunLogBuffer (registered via prepareRunLogNoFile in detached /
// ensure-source mode). The buffer fans out to live WS subscribers and
// tracks its own running offset, so the tailer's file offset is ignored
// here.
//
// Mirrors startEventSource but operates on opaque byte streams rather
// than line-delimited JSON.
func startLogSource(s *Service, runID string, done <-chan struct{}) {
	path := filepath.Join(s.storeDir, "runs", runID, "run.log")
	emit := func(_ int64, chunk []byte) {
		if buf := s.GetLogBuffer(runID); buf != nil {
			_, _ = buf.Write(chunk)
		}
	}
	go runstream.TailLogFile(path, done, emit, s.logger)
}
