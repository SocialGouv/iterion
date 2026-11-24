package mongo

import (
	"bytes"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestRunMetadataSaveBirthCannotAdoptIncumbent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		format int
		want   error
	}{
		{"legacy-zero-version", 1, store.ErrRunConflict},
		{"exact", 2, store.ErrValueFormatChanged},
		{"future", 99, store.ErrUnsupportedValueFormat},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			gate := valueFormatBSONBarrier{make(chan struct{}), make(chan struct{})}
			pending := &store.Run{ID: "r", WorkflowName: "pending-birth", Status: store.RunStatusQueued, Inputs: map[string]any{"gate": gate}}
			done := make(chan error, 1)
			go func() { done <- s.SaveRun(ctx, pending) }()
			<-gate.entered
			released := false
			defer func() {
				if !released {
					close(gate.release)
				}
			}()
			if tt.format == 1 {
				if _, err := s.CreateRun(ctx, "r", "incumbent", nil); err != nil {
					t.Fatal(err)
				}
				// A legitimate pre-CAS legacy document remains supported by SaveRun;
				// a fresh birth that observed absence must not adopt it in its upsert.
				if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$unset": bson.M{"version": ""}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.CreateRunWithValueFormat(ctx, mongoExactBirth("r")); err != nil {
					t.Fatal(err)
				}
				if tt.format == 99 {
					if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
			if err != nil {
				t.Fatal(err)
			}
			close(gate.release)
			released = true
			if err = <-done; !errors.Is(err, tt.want) || !errors.Is(err, store.ErrRunConflict) {
				t.Fatalf("late birth error=%v want %v + conflict", err, tt.want)
			}
			after, err := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("late birth mutated incumbent: %v", err)
			}
		})
	}
}
