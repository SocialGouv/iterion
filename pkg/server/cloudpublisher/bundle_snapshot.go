package cloudpublisher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Snapshots share the existing out-of-band JSON blob transport with compiled
// IR. Their key includes the content digest, so a resume cannot overwrite an
// earlier queued attempt's collection (nor its separate IR object).
func (p *Publisher) offloadBundleSnapshot(ctx context.Context, msg *queue.RunMessage) error {
	ref := msg.BotBundle
	if ref == nil || len(ref.Snapshot) == 0 || ref.SnapshotRef != nil || p.maxPayload == nil {
		return nil
	}
	limit := p.maxPayload()
	if limit <= 0 {
		return nil
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if int64(len(body)) <= limit {
		return nil
	}
	blobs := store.AsIRBlobStore(p.store)
	if blobs == nil {
		return fmt.Errorf("cloudpublisher: bundle snapshot exceeds queue payload and blob store is unavailable")
	}
	backend, err := irBackendForName(blobs.IRBlobBackend())
	if err != nil {
		return err
	}
	key, err := blobs.PutIRBlob(ctx, msg.RunID+"-bundle-"+ref.SnapshotDigest, ref.Snapshot)
	if err != nil {
		return fmt.Errorf("cloudpublisher: store immutable bundle snapshot: %w", err)
	}
	ref.Snapshot, ref.SnapshotRef = nil, &queue.IRRef{StorageKey: key, Backend: backend}
	return nil
}
