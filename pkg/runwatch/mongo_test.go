package runwatch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func newTestMongoWatchStore(t *testing.T) *MongoStore {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_runwatch_" + hex.EncodeToString(nonce))
	t.Logf("Mongo watch database: %s", db.Name())
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	s := NewMongoStore(db)
	if err := s.EnsureSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMongoCompletionRollsBackWhenWatchAccountingFails(t *testing.T) {
	s := newTestMongoWatchStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	w, ep := seedWatchEpisode(t, s, now, "w", "t")
	if _, won, err := s.ClaimEpisode(ctx, ep.ID, "worker", now, time.Minute); err != nil || !won {
		t.Fatalf("claim won=%v err=%v", won, err)
	}
	validator := func(rule bson.M) {
		t.Helper()
		if err := s.watches.Database().RunCommand(ctx, bson.D{
			{Key: "collMod", Value: watchesCollection}, {Key: "validator", Value: rule},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Only the second write fails: the episode CAS is valid, while this
	// collection rejects incrementing the watch's count from zero to one.
	validator(bson.M{"delivered_episodes": bson.M{"$lte": 0}})
	if err := s.CompleteEpisode(ctx, ep.ID, "worker", now.Add(time.Second)); err == nil {
		t.Fatal("completion unexpectedly passed watch validator")
	}
	var got Episode
	if err := s.episodes.FindOne(ctx, bson.M{"_id": ep.ID}).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.State != EpisodeProcessing || got.LeaseOwner != "worker" || got.DeliveredAt != nil {
		t.Fatalf("episode was not rolled back: %+v", got)
	}
	watch, err := s.GetWatch(ctx, w.ID)
	if err != nil || watch.DeliveredEpisodes != 0 || watch.LastDeliveredAt != nil {
		t.Fatalf("watch changed after aborted completion: %+v err=%v", watch, err)
	}
	validator(bson.M{})
	if err := s.CompleteEpisode(ctx, ep.ID, "worker", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEpisode(ctx, ep.ID, "worker", now.Add(2*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat completion = %v", err)
	}
	watch, err = s.GetWatch(ctx, w.ID)
	if err != nil || watch.DeliveredEpisodes != 1 || watch.LastDeliveredAt == nil || !watch.LastDeliveredAt.Equal(now.Add(time.Second)) {
		t.Fatalf("retry accounting = %+v err=%v", watch, err)
	}
}
