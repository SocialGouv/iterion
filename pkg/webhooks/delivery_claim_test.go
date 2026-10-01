package webhooks

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// claimed_at is what every replica ages a launch in flight by, so Mongo must
// carry it: the memory store keeps the struct and ignores its tags, so only
// the document itself can show a tag that drops or renames the field.
func TestDeliveryClaimedAtIsPersisted(t *testing.T) {
	claimed := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	d := Delivery{ID: "d", Status: StatusAccepted, ReceivedAt: claimed.Add(-20 * time.Minute), ClaimedAt: &claimed}
	raw, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["claimed_at"]; !ok {
		t.Fatalf("the document carries no claimed_at: %v", doc)
	}
	var back Delivery
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ClaimedAt == nil || !back.ClaimedAt.Equal(claimed) || !back.ClaimStart().Equal(claimed) {
		t.Errorf("claimed_at did not round-trip: %v", back.ClaimedAt)
	}

	d.ClaimedAt = nil
	raw, err = bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	doc = bson.M{}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["claimed_at"]; ok {
		t.Errorf("an unset claim time is written: %v", doc["claimed_at"])
	}
	// A row written before claimed_at existed dates its claim by its receipt.
	var old Delivery
	if err := bson.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if !old.ClaimStart().Equal(d.ReceivedAt) {
		t.Errorf("a row with no claim time starts at %s, want its receipt %s", old.ClaimStart(), d.ReceivedAt)
	}
}
