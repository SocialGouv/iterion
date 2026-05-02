package runview

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LogSlice is a best-effort timestamp-windowed slice of run.log for a
// single ExecutionState. The free-form log format the iterion logger
// writes (HH:MM:SS in the host's local TZ + emoji + free text) carries
// no node ID, so the slice is derived from the execution's
// [StartedAt, FinishedAt] range. Multi-branch concurrent runs may
// interleave lines from sibling executions in this window — consumers
// should treat the slice as a hint, not ground truth.
type LogSlice struct {
	BestEffort bool      `json:"best_effort"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	StartByte  int64     `json:"start_byte,omitempty"`
	EndByte    int64     `json:"end_byte,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	Notes      []string  `json:"notes,omitempty"`
	Body       string    `json:"body,omitempty"`
}

// BuildLogSlice extracts the timestamp-windowed slice of run.log
// matching exec. The window is [exec.StartedAt, exec.FinishedAt]; if
// FinishedAt is nil, the upper bound is open (read to EOF).
//
// The log timestamp prefix is interpreted in time.Local (matching what
// the iterion logger writes via time.Now().Format("15:04:05")), so the
// reference date for each line is the local-TZ calendar date of the
// execution's start. We track day rollovers within the file by
// detecting backwards jumps in HH:MM:SS (e.g. 23:59:59 → 00:00:00)
// and incrementing the working date.
//
// When tail > 0, the matched region is kept in a rolling tail buffer
// (last tail lines) instead of accumulating the full window in memory
// and trimming at the end. Truncation is reported in the result.
func BuildLogSlice(storeDir, runID string, exec *ExecutionState, tail int) *LogSlice {
	if exec == nil {
		return nil
	}
	if exec.StartedAt == nil {
		return nil
	}

	path := filepath.Join(storeDir, "runs", runID, "run.log")
	f, err := os.Open(path)
	if err != nil {
		// Missing log is not an error — the run may not have produced
		// any log output yet (or the file was pruned). Return a
		// zero-body slice so the caller can still surface the window
		// metadata to the user.
		return &LogSlice{
			BestEffort: true,
			StartTime:  *exec.StartedAt,
			EndTime:    derefTime(exec.FinishedAt),
			Notes:      []string{"run.log not found"},
		}
	}
	defer f.Close()

	loc := time.Local
	startLocal := exec.StartedAt.In(loc)
	var endLocal time.Time
	if exec.FinishedAt != nil {
		endLocal = exec.FinishedAt.In(loc)
	}
	openEnd := exec.FinishedAt == nil

	// Reference date for the next line's timestamp. Starts at the
	// local date of the exec's start; advanced when we detect a
	// midnight rollover.
	day := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
	var lastTOD time.Time

	// Tail buffer (rolling). When tail <= 0 we accumulate everything.
	var lines []string
	var bytesIn int64
	var startByte, endByte int64 = -1, -1
	truncated := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var offset int64
	for scanner.Scan() {
		line := scanner.Text()
		lineLen := int64(len(line)) + 1 // +1 for the newline scanner stripped
		lineStart := offset
		offset += lineLen

		tod, ok := parseLogTimeOfDay(line)
		if !ok {
			// Continuation / non-prefixed line: attribute it to the
			// previous parsed timestamp if we are currently inside the
			// window. Without lastTOD we cannot place it; skip.
			if !lastTOD.IsZero() {
				ts := combineDayTOD(day, lastTOD)
				if inWindow(ts, startLocal, endLocal, openEnd) {
					addLine(&lines, line, tail, &truncated)
					if startByte < 0 {
						startByte = lineStart
					}
					endByte = offset
					bytesIn += lineLen
				}
			}
			continue
		}

		// Detect midnight rollover: a parsed time-of-day strictly
		// earlier than the previous one is treated as the next day.
		// The 12h "swing back" guard suppresses pathological cases
		// like a clock skew of a few seconds being misread as a day
		// jump.
		if !lastTOD.IsZero() && tod.Before(lastTOD) {
			diff := lastTOD.Sub(tod)
			if diff > 12*time.Hour {
				day = day.Add(24 * time.Hour)
			}
		}
		lastTOD = tod

		ts := combineDayTOD(day, tod)
		if !inWindow(ts, startLocal, endLocal, openEnd) {
			continue
		}
		addLine(&lines, line, tail, &truncated)
		if startByte < 0 {
			startByte = lineStart
		}
		endByte = offset
		bytesIn += lineLen
	}
	// scanner.Err() is intentionally ignored — a partial read still
	// yields a useful slice.

	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}

	notes := []string{}
	if truncated {
		notes = append(notes, "truncated to last "+itoa(tail)+" lines")
	}

	out := &LogSlice{
		BestEffort: true,
		StartTime:  *exec.StartedAt,
		EndTime:    derefTime(exec.FinishedAt),
		Body:       body,
		Truncated:  truncated,
		Notes:      notes,
	}
	if startByte >= 0 {
		out.StartByte = startByte
		out.EndByte = endByte
	}
	return out
}

// parseLogTimeOfDay extracts the leading "HH:MM:SS " token from a log
// line and returns its time-of-day on day zero (year 0). Returns ok=false
// when the line does not start with a parseable timestamp.
func parseLogTimeOfDay(line string) (time.Time, bool) {
	if len(line) < 8 {
		return time.Time{}, false
	}
	// Expect HH:MM:SS at positions 0..7. Bail out cheaply on the colons.
	if line[2] != ':' || line[5] != ':' {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("15:04:05", line[:8], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// combineDayTOD builds an instant from a date (in time.Local) and a
// time-of-day parsed at year 0. Hour/minute/second come from tod, the
// rest from day.
func combineDayTOD(day, tod time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), tod.Hour(), tod.Minute(), tod.Second(), 0, day.Location())
}

// inWindow returns true when ts is within [startLocal, endLocal]. When
// openEnd is true the upper bound is ignored.
func inWindow(ts, startLocal, endLocal time.Time, openEnd bool) bool {
	if ts.Before(startLocal) {
		return false
	}
	if openEnd {
		return true
	}
	return !ts.After(endLocal)
}

// addLine appends line to lines; when tail > 0 the buffer is bounded
// (oldest entry dropped) and *truncated is set when the cap is hit.
func addLine(lines *[]string, line string, tail int, truncated *bool) {
	if tail > 0 && len(*lines) >= tail {
		*lines = (*lines)[1:]
		*truncated = true
	}
	*lines = append(*lines, line)
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// itoa avoids strconv import for a single small use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
