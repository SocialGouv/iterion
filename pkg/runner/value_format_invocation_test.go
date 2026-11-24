package runner

import (
	"bytes"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerExecutionPreparationPrecedesIRAndWorkspace(t *testing.T) {
	t.Parallel()
	for _, actual := range []store.ValueFormat{0, 1, 2} {
		t.Run(string(rune('0'+actual)), func(t *testing.T) {
			t.Parallel()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if actual != 0 {
				if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: actual}); err != nil {
					t.Fatal(err)
				}
			}
			from := store.LegacyValueFormat
			if actual != 0 {
				from = 3 - actual
			}
			ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "run", Format: from})
			if err != nil {
				t.Fatal(err)
			}
			doc := filepath.Join(st.Root(), "runs", "run", "run.json")
			before, _ := os.ReadFile(doc)
			work := filepath.Join(t.TempDir(), "not-created")
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop(), WorkDir: work}}
			var usage *metricsEmitter
			err = r.executeRun(ctx, &queue.RunMessage{RunID: "run", IRCompiled: []byte("malformed IR which must not be read")}, &usage, nil)
			want := store.ErrValueFormatChanged
			if actual == 0 {
				want = store.ErrRunNotFound
			}
			if !errors.Is(err, want) || usage != nil {
				t.Fatalf("error=%v want %v usage=%v", err, want, usage)
			}
			after, _ := os.ReadFile(doc)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed successor")
			}
			if _, err = os.Stat(work); !os.IsNotExist(err) {
				t.Fatalf("workspace touched: %v", err)
			}
		})
	}
}
