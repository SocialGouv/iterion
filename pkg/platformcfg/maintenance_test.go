package platformcfg

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMaintenancePersistence(t *testing.T) {
	st := NewMemoryStore[Maintenance]()
	ctx := context.Background()
	if rec, err := st.Get(ctx); err != nil || rec != nil {
		t.Fatalf("default: %+v %v", rec, err)
	}
	if err := st.Put(ctx, Maintenance{FenceLaunches: true, Reason: "migration", UpdatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.FenceLaunches || rec.UpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("stamp: %+v", rec)
	}
	raw, err := bson.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["fence_launches"] != true || doc["reason"] != "migration" || doc["updated_at"] == nil {
		t.Fatalf("Mongo wire: %v", doc)
	}
	if ok, err := st.PutIfUnchanged(ctx, Maintenance{Reason: "done"}, rec.UpdatedAt); err != nil || !ok {
		t.Fatalf("CAS: %v %v", ok, err)
	}
	rec, err = st.Get(ctx)
	if err != nil || rec.FenceLaunches {
		t.Fatalf("disarmed: %+v %v", rec, err)
	}
}
