package nats

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrRunnerEpochSuperseded marks a process whose literal PodTemplate epoch is
// below the durable fleet high-water mark. It is intentionally a sentinel so
// entrypoints and API layers can surface the condition without string matching.
var ErrRunnerEpochSuperseded = errors.New("queue/nats: runner epoch superseded")

func (c *Conn) stampRunnerEpoch(msg *queue.RunMessage) error {
	if c == nil {
		return fmt.Errorf("queue/nats: connection not initialised")
	}
	if c.superseded {
		return fmt.Errorf("%w: self=%d high_water=%d", ErrRunnerEpochSuperseded, c.cfg.RunnerEpoch, c.highWaterEpoch)
	}
	if msg == nil {
		return fmt.Errorf("queue/nats: invalid RunMessage: queue: nil RunMessage")
	}
	msg.RunnerEpoch = c.cfg.RunnerEpoch
	return nil
}

// epochKV is the minimal CAS surface used by the monotonic startup gate. The
// narrow interface keeps the race behaviour unit-testable without a broker.
type epochKV interface {
	Get(context.Context, string) (jetstream.KeyValueEntry, error)
	Create(context.Context, string, []byte, ...jetstream.KVCreateOpt) (uint64, error)
	Update(context.Context, string, []byte, uint64) (uint64, error)
}

// reconcileRunnerEpoch creates or monotonically advances the high-water mark.
// A lower self epoch is reported as superseded without mutating the mark.
func reconcileRunnerEpoch(ctx context.Context, kv epochKV, self uint64) (highWater uint64, superseded bool, err error) {
	if kv == nil {
		return 0, false, fmt.Errorf("rollout KV is not initialised")
	}
	want := []byte(strconv.FormatUint(self, 10))
	for {
		entry, getErr := kv.Get(ctx, RunnerEpochHighWaterKey)
		if errors.Is(getErr, jetstream.ErrKeyNotFound) {
			if _, createErr := kv.Create(ctx, RunnerEpochHighWaterKey, want); createErr == nil {
				return self, false, nil
			} else if errors.Is(createErr, jetstream.ErrKeyExists) {
				continue
			} else {
				return 0, false, fmt.Errorf("create high-water mark: %w", createErr)
			}
		}
		if getErr != nil {
			return 0, false, fmt.Errorf("read high-water mark: %w", getErr)
		}
		observed, parseErr := strconv.ParseUint(strings.TrimSpace(string(entry.Value())), 10, 64)
		if parseErr != nil {
			return 0, false, fmt.Errorf("parse high-water mark %q: %w", entry.Value(), parseErr)
		}
		switch {
		case self < observed:
			return observed, true, nil
		case self == observed:
			return observed, false, nil
		default:
			if _, updateErr := kv.Update(ctx, RunnerEpochHighWaterKey, want, entry.Revision()); updateErr == nil {
				return self, false, nil
			} else if errors.Is(updateErr, jetstream.ErrKeyRevisionMismatch) {
				// Another process won the CAS. Re-read: it may have advanced
				// above us, in which case this process is now superseded.
				continue
			} else {
				return 0, false, fmt.Errorf("advance high-water mark from %d to %d: %w", observed, self, updateErr)
			}
		}
	}
}
