package dispatcher

import (
	"errors"
	"fmt"
	"testing"
)

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
