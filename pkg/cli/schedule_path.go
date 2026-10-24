package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	scheduleSystemPath   = "/usr/local/bin:/usr/bin:/bin"
	schedulePathMaxBytes = 8192
	schedulePathLimitEnv = "ITERION_SCHEDULE_PATH_MAX_BYTES"
)

func schedulePathLimit() (int, error) {
	value := os.Getenv(schedulePathLimitEnv)
	if value == "" {
		return schedulePathMaxBytes, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 || (limit > 0 && limit < len(scheduleSystemPath)) {
		return 0, fmt.Errorf("%s must be 0 (unlimited) or at least %d bytes, got %q", schedulePathLimitEnv, len(scheduleSystemPath), value)
	}
	return limit, nil
}

type droppedSchedulePath struct {
	path   string
	reason string
}

// sanitizeSchedulePath preserves search precedence, then removes whole entries
// from the tail to meet limit (0 disables trimming). System directories always
// survive, including ones already at the tail of the install-time PATH.
func sanitizeSchedulePath(raw string, limit int) (string, []droppedSchedulePath) {
	// return raw, nil // Uncomment to redden W2's filtering and fallback cases.
	var entries []string
	var dropped []droppedSchedulePath
	seen := make(map[string]bool)
	for _, entry := range strings.Split(raw, string(os.PathListSeparator)) {
		reason := ""
		switch {
		case entry == "":
			reason = "empty"
		case !filepath.IsAbs(entry):
			reason = "relative"
		case strings.ContainsAny(entry, "\r\n\x00"):
			reason = "control character"
		case seen[entry]:
			reason = "duplicate"
		}
		if reason != "" {
			dropped = append(dropped, droppedSchedulePath{entry, reason})
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	protected := make(map[string]bool)
	for _, entry := range strings.Split(scheduleSystemPath, ":") {
		protected[entry] = true
		if !seen[entry] {
			entries = append(entries, entry)
		}
	}
	size := len(strings.Join(entries, string(os.PathListSeparator)))
	// clear(protected) // Uncomment to redden W2's system-tail case.
	for i := len(entries) - 1; limit > 0 && size > limit && i >= 0; i-- {
		if protected[entries[i]] {
			continue
		}
		dropped = append(dropped, droppedSchedulePath{entries[i], "size limit"})
		size -= len(entries[i]) + 1
		entries = append(entries[:i], entries[i+1:]...)
	}
	return strings.Join(entries, string(os.PathListSeparator)), dropped
}

func renderScheduleWrapper(bin, originalPath, cleanPath string, limit int) string {
	return fmt.Sprintf("#!/bin/sh\n# Managed by iterion schedule install.\n# PATH bytes: original=%d sanitized=%d limit=%d (0=unlimited).\nPATH=%s\nexport PATH\nexec %s \"$@\"\n",
		len(originalPath), len(cleanPath), limit, shellQuote(cleanPath), shellQuote(bin))
}

// installScheduleCron publishes the wrapper before cron can reference it. If
// cron rejects the replacement, restore the wrapper used by the existing block.
func installScheduleCron(path, wrapper, crontab string) error {
	previous, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read schedule wrapper %s: %w", path, err)
	}
	var mode os.FileMode
	if existed {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat schedule wrapper %s: %w", path, err)
		}
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(path, []byte(wrapper), 0o700); err != nil {
		return fmt.Errorf("write schedule wrapper %s: %w", path, err)
	}
	if err := crontabWriter(crontab); err != nil {
		var restoreErr error
		if existed {
			restoreErr = writeFileAtomic(path, previous, mode)
		} else {
			restoreErr = os.Remove(path)
		}
		if restoreErr != nil {
			return fmt.Errorf("install crontab: %w (restore schedule wrapper %s: %v)", err, path, restoreErr)
		}
		return err
	}
	return nil
}
