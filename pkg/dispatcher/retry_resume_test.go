package dispatcher

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestLastRunForbidsFresh(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusRunning,
		store.RunStatusQueued,
		store.RunStatusFinished,
		store.RunStatusFailedResumable,
		store.RunStatusCancelled,
	} {
		if !lastRunForbidsFresh(status) {
			t.Errorf("lastRunForbidsFresh(%s) = false, want true", status)
		}
	}
	if lastRunForbidsFresh(store.RunStatusFailed) {
		t.Error("hard-failed last_run may start fresh when the ticket is explicitly eligible")
	}
	if lastRunForbidsFresh("") {
		t.Error("unknown/unreadable status must not block a fresh run")
	}
}

func TestIsResumeSourceChanged(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("delegate: claude-code failed: context canceled"), false},
		{
			"runtime source-changed verbatim",
			fmt.Errorf(`runtime: workflow source has changed since run "019e6dd0" was started (expected hash 80fcb275d074, got 31e3bb64518a); re-run from scratch or use --force`),
			true,
		},
		{
			"wrapped source-changed",
			fmt.Errorf("dispatch run failed: %w", errors.New("runtime: workflow source has changed since run X was started")),
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isResumeSourceChanged(c.err); got != c.want {
				t.Errorf("isResumeSourceChanged(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
