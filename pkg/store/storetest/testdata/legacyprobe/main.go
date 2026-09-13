// This fixture is compiled against the supported OLD source tree, never the
// current module. It calls the old public Store and S3 APIs so confinement
// tests exercise executable readers and mutators, including blind deletes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	storemongo "github.com/SocialGouv/iterion/pkg/store/mongo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", "", "isolated filesystem store / scratch root")
	id := flag.String("id", "", "run ID")
	action := flag.String("action", "", "read, list, write, repair, delete or prune")
	message := flag.String("message", "", "queue JSON to decode before touching any store")
	uri := flag.String("mongo", "", "test Mongo URI")
	database := flag.String("database", "", "test Mongo database")
	endpoint := flag.String("s3", "", "test S3 HTTP endpoint")
	bucket := flag.String("bucket", "", "test S3 bucket")
	natsURI := flag.String("nats", "", "disposable NATS URI")
	rolloutBucket := flag.String("rollout-bucket", "", "isolated rollout KV bucket")
	flag.Parse()
	if *action == "queue-check" {
		var m queue.RunMessage
		if err := json.Unmarshal([]byte(*message), &m); err != nil {
			return err
		}
		return m.Validate()
	}
	ctx, cancel := context.WithTimeout(store.WithoutTenantFilter(context.Background()), 30*time.Second)
	defer cancel()
	if *action == "queue-schema" {
		conn, err := natsq.Connect(ctx, natsq.Config{URL: *natsURI, RolloutKVBucket: *rolloutBucket})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}
	var s store.RunStore
	var blobs blob.Client
	if *uri != "" {
		var err error
		blobs, err = blob.NewS3(ctx, blob.Config{Endpoint: *endpoint, Region: "us-east-1", Bucket: *bucket, UsePathStyle: true, AccessKeyID: "test-key", SecretAccessKey: "test-secret"})
		if err != nil {
			return err
		}
		defer blobs.Close()
		mongo, err := storemongo.New(ctx, storemongo.Config{URI: *uri, Database: *database, Blob: blobs, RunFilesScratchDir: *root})
		if err != nil {
			return err
		}
		defer mongo.Close(ctx)
		s = mongo
	} else {
		var err error
		s, err = store.New(*root)
		if err != nil {
			return err
		}
	}
	switch *action {
	case "read":
		r, err := s.LoadRun(ctx, *id)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(r)
	case "list":
		ids, err := s.ListRuns(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(ids)
	case "write":
		r, err := s.CreateRun(ctx, *id, "legacy-shadow", map[string]any{"source": "old writer"})
		if err != nil {
			return err
		}
		r.Name = "old full-document save"
		if err := s.SaveRun(ctx, r); err != nil {
			return err
		}
		if _, err := s.AppendEvent(ctx, *id, store.Event{Type: store.EventNodeStarted, NodeID: "node"}); err != nil {
			return err
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{RunID: *id, NodeID: "node", Version: 1, Data: map[string]any{"value": "old"}}); err != nil {
			return err
		}
		if err := s.WriteInteraction(ctx, &store.Interaction{ID: "question", RunID: *id, NodeID: "node"}); err != nil {
			return err
		}
		if err := s.WriteAttachment(ctx, *id, store.AttachmentRecord{Name: "input", OriginalFilename: "input.txt"}, bytes.NewReader([]byte("old attachment"))); err != nil {
			return err
		}
		if _, err := store.AsToolBlobStore(s).WriteToolBlob(ctx, *id, "call", "output", []byte("old tool")); err != nil {
			return err
		}
		if err := store.AsBackendSessionStore(s).PutBackendSession(ctx, *id, "session", []byte("old session")); err != nil {
			return err
		}
		if blobs != nil {
			if err := blobs.PutIRBlob(ctx, *id, []byte("old IR")); err != nil {
				return err
			}
			if err := blobs.PutRunFile(ctx, *id, "report.txt", "text/plain", bytes.NewReader([]byte("old file")), 8); err != nil {
				return err
			}
		}
		return nil
	case "repair":
		if err := s.SaveCheckpoint(ctx, *id, &store.Checkpoint{NodeID: "node"}); err != nil {
			return err
		}
		return s.UpdateRunStatus(ctx, *id, store.RunStatusFinished, "")
	case "delete":
		return s.DeleteRun(ctx, *id)
	case "prune":
		_, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
		return err
	default:
		return fmt.Errorf("unknown legacy probe action %q", *action)
	}
}
