package schedgate

import (
	"fmt"
	"os"
	"path/filepath"
)

// LocalAuditPathFor places the tick-audit JSONL next to the schedule
// manifest's cron logs: <manifest-dir>/logs/tick-audit.jsonl. Shared by
// the host-cron surface (which knows its manifest path exactly) and
// DefaultLocalAuditPath below.
func LocalAuditPathFor(manifestPath string) string {
	return filepath.Join(filepath.Dir(manifestPath), "logs", "tick-audit.jsonl")
}

// DefaultLocalAuditPath resolves the local tick-audit file the way the
// schedule CLI resolves its manifest (ITERION_SCHEDULES_FILE override,
// else ~/.iterion/schedules.yaml), so the in-process trigger scheduler
// writes to the SAME file `iterion schedule audit` reads.
func DefaultLocalAuditPath() (string, error) {
	if env := os.Getenv("ITERION_SCHEDULES_FILE"); env != "" {
		return LocalAuditPathFor(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("schedgate: resolve home dir for tick audit: %w", err)
	}
	return LocalAuditPathFor(filepath.Join(home, ".iterion", "schedules.yaml")), nil
}
