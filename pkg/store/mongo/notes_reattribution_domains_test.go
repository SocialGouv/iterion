package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const reattributionFromAuthor = "alice+review@example.org"

func seedNoteReattributionDomains(t *testing.T, s *Store, c context.Context) {
	t.Helper()
	for _, coll := range []*mongo.Collection{s.runNotes, s.db.Collection(colIncarnationNotes)} {
		for i, a := range []string{reattributionFromAuthor, " ALICE+REVIEW@EXAMPLE.ORG ", "alice+review@example.org.uk", "alicereview@example.org"} {
			tenant := "tenant-A"
			if i == 1 {
				tenant = "tenant-B"
			}
			row := bson.M{"run_id": "same", "seq": i, "tenant_id": tenant, "data_namespace": fmt.Sprintf("retired-or-orphan-%d", i), "author": a, "body": fmt.Sprintf("body-%d", i)}
			if _, e := coll.InsertOne(c, row); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func countReattributedAuthor(t *testing.T, c context.Context, coll *mongo.Collection, author string) int64 {
	t.Helper()
	n, e := coll.CountDocuments(c, bson.M{"author": author})
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func TestRunNoteReattributePhysicalDomains(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			seedNoteReattributionDomains(t, s, c)
			call := context.Background()
			if scoped {
				call = store.WithIdentity(call, "tenant-A", "operator")
			}
			n, e := s.ReattributeRunNotes(call, " ALICE+REVIEW@example.org ", "erased")
			if e != nil || n != 4 {
				t.Fatalf("global alias %d %v", n, e)
			}
			for _, coll := range []*mongo.Collection{s.runNotes, s.db.Collection(colIncarnationNotes)} {
				if n := countReattributedAuthor(t, c, coll, "erased"); n != 2 {
					t.Fatalf("domain %s erased%d", coll.Name(), n)
				}
				for _, author := range []string{"alice+review@example.org.uk", "alicereview@example.org"} {
					if countReattributedAuthor(t, c, coll, author) != 1 {
						t.Fatal("regex match widened", author)
					}
				}
				for i := 0; i < 4; i++ {
					n, e := coll.CountDocuments(c, bson.M{"seq": i, "body": fmt.Sprintf("body-%d", i)})
					if e != nil || n != 1 {
						t.Fatal("body/seq changed", n, e)
					}
				}
			}
			n, e = s.ReattributeRunNotes(call, reattributionFromAuthor, "erased")
			if e != nil || n != 0 {
				t.Fatal("repeat", n, e)
			}
			n, e = s.ReattributeRunNotes(call, "  ", "should-not-write")
			if e == nil || n != 0 {
				t.Fatal("blank accepted", n, e)
			}
			count, e := s.runs.CountDocuments(c, bson.M{})
			if e != nil || count != 0 {
				t.Fatal("created header", count, e)
			}
		})
	}
}
func TestRunNoteReattributePartialDomainFailure(t *testing.T) {
	for _, mode := range []string{"legacy", "strict", "both"} {
		t.Run(mode, func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			seedNoteReattributionDomains(t, s, c)
			colls := []*mongo.Collection{s.runNotes, s.db.Collection(colIncarnationNotes)}
			for i, coll := range colls {
				if mode == "both" || mode == "legacy" && i == 0 || mode == "strict" && i == 1 {
					if e := s.db.RunCommand(c, bson.D{{Key: "collMod", Value: coll.Name()}, {Key: "validator", Value: bson.M{"author": bson.M{"$ne": "erased"}}}}).Err(); e != nil {
						t.Fatal(e)
					}
				}
			}
			n, e := s.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
			want := int64(2)
			if mode == "both" {
				want = 0
			}
			if e == nil || n != want {
				t.Fatalf("partial n%d want%d err%v", n, want, e)
			}
			var we mongo.WriteException
			if !errors.As(e, &we) {
				t.Fatalf("lost driver error: %v", e)
			}
			for i, coll := range colls {
				fail := mode == "both" || mode == "legacy" && i == 0 || mode == "strict" && i == 1
				if fail && !strings.Contains(e.Error(), coll.Name()) {
					t.Fatal("domain missing", e)
				}
				if e := s.db.RunCommand(c, bson.D{{Key: "collMod", Value: coll.Name()}, {Key: "validator", Value: bson.M{}}}).Err(); e != nil {
					t.Fatal(e)
				}
			}
			n, e = s.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
			if e != nil || n != 4-want {
				t.Fatalf("retry n%d want%d err%v", n, 4-want, e)
			}
			n, e = s.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
			if e != nil || n != 0 {
				t.Fatal(n, e)
			}
		})
	}
}
func TestRunNoteReattributeCancellationBetweenDomains(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	seedNoteReattributionDomains(t, s, c)
	call, cancel := context.WithCancel(c)
	defer cancel()
	var updates atomic.Int32
	client, e := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetMonitor(&event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "update" && updates.Add(1) == 1 {
			cancel()
		}
	}}))
	if e != nil {
		t.Fatal(e)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(s.db.Name())
	target := &Store{db: db, runNotes: db.Collection(colRunNotes)}
	n, e := target.ReattributeRunNotes(call, reattributionFromAuthor, "erased")
	if n != 2 || !errors.Is(e, context.Canceled) || updates.Load() != 1 {
		t.Fatalf("cancel n%d err%v commands%d", n, e, updates.Load())
	}
	if countReattributedAuthor(t, c, s.db.Collection(colIncarnationNotes), "erased") != 0 {
		t.Fatal("second domain ran")
	}
	n, e = s.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
	if e != nil || n != 2 {
		t.Fatal("retry", n, e)
	}
}
func TestRunNoteReattributeUnacknowledgedOutcome(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	seedNoteReattributionDomains(t, s, c)
	if e := s.db.RunCommand(c, bson.D{{Key: "collMod", Value: colRunNotes}, {Key: "validator", Value: bson.M{"author": bson.M{"$ne": "erased"}}}}).Err(); e != nil {
		t.Fatal(e)
	}
	parsed, parseErr := url.Parse(os.Getenv("ITERION_TEST_MONGO_URI"))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	query := parsed.Query()
	for key := range query {
		if strings.EqualFold(key, "w") || strings.EqualFold(key, "j") || strings.EqualFold(key, "journal") {
			query.Del(key)
		}
	}
	query.Set("w", "0")
	parsed.RawQuery = query.Encode()
	uri := parsed.String()
	u, e := New(c, Config{URI: uri, Database: s.db.Name(), Blob: newInMemoryBlob(), RunFilesScratchDir: t.TempDir()})
	if e != nil {
		t.Fatal("configured store refused w0", e)
	}
	defer u.Close(context.Background())
	n, e := u.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
	t.Logf("configured w0 result count=%d err=%v", n, e)
	if e == nil || n != 0 || !strings.Contains(e.Error(), "unacknowledged write outcome") {
		t.Fatalf("unacknowledged outcome silently accepted n%d err%v", n, e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for countReattributedAuthor(t, c, s.db.Collection(colIncarnationNotes), "erased") != 2 {
		if time.Now().After(deadline) {
			t.Fatal("unack strict update never observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if countReattributedAuthor(t, c, s.runNotes, "erased") != 0 {
		t.Fatal("validator unexpectedly accepted legacy")
	}
	t.Log("real New(w=0) accepted; legacy failed validation, strict changed2, caller receives explicit unknown-outcome error")
}
func TestRunNoteReattributeAcknowledgedCount(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	seedNoteReattributionDomains(t, s, c)
	if e := s.db.RunCommand(c, bson.D{{Key: "collMod", Value: colRunNotes}, {Key: "validator", Value: bson.M{"$or": bson.A{bson.M{"seq": bson.M{"$ne": 1}}, bson.M{"author": bson.M{"$ne": "erased"}}}}}}).Err(); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	var acknowledged atomic.Int64
	client, e := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetMonitor(&event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "update" && calls.Add(1) == 1 {
			n, _ := e.Reply.Lookup("nModified").AsInt64OK()
			acknowledged.Store(n)
		}
	}}))
	if e != nil {
		t.Fatal(e)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(s.db.Name())
	target := &Store{db: db, runNotes: db.Collection(colRunNotes)}
	n, e := target.ReattributeRunNotes(c, reattributionFromAuthor, "erased")
	if e == nil || n != 2+acknowledged.Load() {
		t.Fatalf("lost acknowledged count n%d ackFirst%d err%v", n, acknowledged.Load(), e)
	}
	t.Logf("failed first command acknowledgedModified=%d physicallyModified=%d; total=%d with error=%v", acknowledged.Load(), countReattributedAuthor(t, c, s.runNotes, "erased"), n, e)
}
