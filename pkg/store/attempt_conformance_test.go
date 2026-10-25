package store_test

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"testing"
)

func TestAttemptSnapshotsFilesystem(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storetest.AttemptSnapshots(t, st)
}
