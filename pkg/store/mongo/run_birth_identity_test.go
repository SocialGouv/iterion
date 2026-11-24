package mongo

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestBirthIdentityLegacyAbsenceRemainsAbsent(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	r, err := s.CreateRun(ctx, "legacy", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$unset": bson.M{"birth_id": ""}}); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Name = "legacy metadata"
	if err = s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BirthID != "" || r.BirthID != "" {
		t.Fatal("save invented legacy birth identity")
	}
	raw, err := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.LookupErr("birth_id"); err == nil {
		t.Fatal("legacy absent identity became present")
	}
}
