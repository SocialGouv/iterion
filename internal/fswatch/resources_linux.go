package fswatch

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// EMFILE from inotify_init has two meanings: the process descriptor ceiling
// or the real UID's inotify instance ceiling. Record observations, not an
// inferred cause: concurrent processes can change either count immediately.
func resourceError(err error) error {
	if !errors.Is(err, syscall.EMFILE) && !errors.Is(err, syscall.ENFILE) && !errors.Is(err, syscall.ENOSPC) {
		return err
	}
	soft, hard := "unavailable", "unavailable"
	var limit unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &limit) == nil {
		soft, hard = strconv.FormatUint(limit.Cur, 10), strconv.FormatUint(limit.Max, 10)
	}
	// One ordinary descriptor, closed immediately, distinguishes what this
	// process can open at the time of the observation without exhausting the
	// shared inotify pool as a "capacity probe" would.
	ordinary := "open_ok"
	f, openErr := os.Open(os.DevNull)
	if openErr != nil {
		ordinary = "open_failed"
		if errors.Is(openErr, syscall.EMFILE) {
			ordinary = "EMFILE"
		}
		if errors.Is(openErr, syscall.ENFILE) {
			ordinary = "ENFILE"
		}
	} else {
		_ = f.Close()
	}
	fdCount := "unavailable"
	if entries, readErr := os.ReadDir("/proc/self/fd"); readErr == nil {
		fdCount = strconv.Itoa(len(entries))
	}
	readLimit := func(name string) string {
		raw, readErr := os.ReadFile("/proc/sys/fs/inotify/" + name)
		if readErr != nil {
			return "unavailable"
		}
		value := strings.TrimSpace(string(raw))
		if _, parseErr := strconv.ParseUint(value, 10, 64); parseErr != nil {
			return "unavailable"
		}
		return value
	}
	return fmt.Errorf("%w [watcher resources: real_uid=%d process_fds=%s nofile_soft=%s nofile_hard=%s ordinary_fd=%s max_user_instances=%s max_user_watches=%s; inotify limits may be shared with other containers using this UID]",
		err, os.Getuid(), fdCount, soft, hard, ordinary, readLimit("max_user_instances"), readLimit("max_user_watches"))
}
