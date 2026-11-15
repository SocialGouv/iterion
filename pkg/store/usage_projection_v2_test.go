package store_test

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"testing"
)

func TestUsageV2ProjectionFS(t *testing.T) {
	storetest.RunUsageProjectionV2(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestUsageV2ProjectionFSUpgrade(t *testing.T) {
	storetest.RunUsageProjectionV2Upgrade(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}

func TestReportedSessionFS(t *testing.T) {
	storetest.RunReportedSession(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
