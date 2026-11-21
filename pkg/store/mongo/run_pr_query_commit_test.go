package mongo

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestRunPRQueryDiscardsCommitFailure(t *testing.T) {
	// The server failpoint is scoped to this unique client application name.
	// It is available only in the owned test fixture, never a live cluster.
	s, base := valueFormatMongo(t, true)
	ctx, cancel := context.WithTimeout(base, 20*time.Second)
	defer cancel()
	if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2, Inputs: map[string]any{"pr_url": "pr"}}); e != nil {
		t.Fatal(e)
	}
	app := "value-pr-commit-" + bsonNonce(t)
	client, e := drivermongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetAppName(app))
	if e != nil {
		t.Fatal(e)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(s.db.Name())
	reader := &Store{client: client, db: db, runs: db.Collection(colRuns)}
	command := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}}, {Key: "data", Value: bson.M{"failCommands": bson.A{"commitTransaction"}, "errorCode": 2, "appName": app}}}
	if e = s.client.Database("admin").RunCommand(ctx, command).Err(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		_ = s.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err()
	}()
	ids, e := reader.ListRunsForPR(ctx, "pr")
	var failure drivermongo.CommandError
	if ids != nil || !errors.As(e, &failure) || failure.Code != 2 {
		t.Fatal("commit failure returned result or masked cause", ids, e)
	}
	// The reader and snapshot mechanism remain usable after the failed commit.
	ids, e = reader.ListRunsForPR(ctx, "pr")
	if e != nil || len(ids) != 1 || ids[0] != "r" {
		t.Fatal(ids, e)
	}
}
