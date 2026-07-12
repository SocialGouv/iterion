package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the per-LLM-turn seam so the studio
// per-node timeline + the fork-from-a-prior-turn UX work identically in
// cloud and local mode. Before this twin, AsTurnStore returned nil for
// cloud runs, so `iterion fork` reported "cloud stores not yet supported"
// and the runner skipped per-turn capture (model/hooks.go turnSink nil).
var _ store.TurnStore = (*Store)(nil)

// runTurnDoc is one persisted TurnCheckpoint, one document per
// (run_id, node_id, loop_iter, turn_index). The cloud twin of the
// filesystem store's runs/<id>/turns/<node>/<iter>/<turn>.json plus its
// sibling <turn>.messages.json — here the claw message blob is stored
// INLINE in the `messages` field (a run's accumulated conversation is
// well under the 16 MiB BSON document ceiling), so a single upsert
// replaces the whole turn atomically. Tenant-stamped like run_plans so a
// tenant only ever reads its own turns; the `ts` date field carries the
// shared derived-observability TTL.
type runTurnDoc struct {
	TenantID     string               `bson:"tenant_id,omitempty"`
	RunID        string               `bson:"run_id"`
	NodeID       string               `bson:"node_id"`
	LoopIter     int                  `bson:"loop_iter"`
	TurnIndex    int                  `bson:"turn_index"`
	Backend      string               `bson:"backend,omitempty"`
	Model        string               `bson:"model,omitempty"`
	FinishReason string               `bson:"finish_reason,omitempty"`
	ToolCalls    []store.TurnToolCall `bson:"tool_calls,omitempty"`
	TextDigest   string               `bson:"text_digest,omitempty"`
	Usage        store.TurnUsage      `bson:"usage,omitempty"`
	SessionID    string               `bson:"session_id,omitempty"`
	MessagesRef  string               `bson:"messages_ref,omitempty"`
	// Messages is the raw claw []api.Message blob captured with the turn,
	// stored inline. The read methods (LoadTurn/ListTurns/LatestTurn/
	// LoadTurnAtIndex) deliberately do NOT surface it — mirroring the fs
	// impl, which never inlines the sibling messages.json — so callers who
	// need it follow up with LoadTurnMessages. Kept as []byte so the store
	// stays agnostic of the backend's wire format.
	Messages  []byte    `bson:"messages,omitempty"`
	GitRef    string    `bson:"git_ref,omitempty"`
	WrittenAt time.Time `bson:"ts"`
}

// toCheckpoint converts the persisted document back to the wire-shape
// store.TurnCheckpoint the timeline + Fork API consume. Messages is left
// nil to match the filesystem reader (only LoadTurnMessages fetches it).
func (d runTurnDoc) toCheckpoint() *store.TurnCheckpoint {
	return &store.TurnCheckpoint{
		RunID:        d.RunID,
		NodeID:       d.NodeID,
		LoopIter:     d.LoopIter,
		TurnIndex:    d.TurnIndex,
		Backend:      d.Backend,
		Model:        d.Model,
		FinishReason: d.FinishReason,
		ToolCalls:    d.ToolCalls,
		TextDigest:   d.TextDigest,
		Usage:        d.Usage,
		SessionID:    d.SessionID,
		MessagesRef:  d.MessagesRef,
		GitRef:       d.GitRef,
		WrittenAt:    d.WrittenAt,
	}
}

// turnKeyFilter builds the tenant-scoped unique-key filter for one turn.
func turnKeyFilter(ctx context.Context, runID, nodeID string, loopIter, turn int) bson.M {
	return withTenantFilter(ctx, bson.M{
		"run_id":     runID,
		"node_id":    nodeID,
		"loop_iter":  loopIter,
		"turn_index": turn,
	})
}

// WriteTurn implements store.TurnStore: idempotent upsert of one turn
// checkpoint keyed on (run_id, node_id, loop_iter, turn_index). Re-writing
// the same key replaces the prior document, mirroring the filesystem
// store's atomic overwrite. t.Messages (when non-nil) is persisted inline.
func (s *Store) WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error {
	if t == nil {
		return fmt.Errorf("store/mongo: WriteTurn: nil turn")
	}
	if t.RunID == "" || t.NodeID == "" {
		return fmt.Errorf("store/mongo: WriteTurn: run_id and node_id are required")
	}
	if t.LoopIter < 0 || t.TurnIndex < 0 {
		return fmt.Errorf("store/mongo: WriteTurn: negative iter/turn (%d/%d)", t.LoopIter, t.TurnIndex)
	}
	if t.WrittenAt.IsZero() {
		t.WrittenAt = time.Now().UTC()
	}
	tenantID, _ := store.TenantFromContext(ctx)
	doc := runTurnDoc{
		TenantID:     tenantID,
		RunID:        t.RunID,
		NodeID:       t.NodeID,
		LoopIter:     t.LoopIter,
		TurnIndex:    t.TurnIndex,
		Backend:      t.Backend,
		Model:        t.Model,
		FinishReason: t.FinishReason,
		ToolCalls:    t.ToolCalls,
		TextDigest:   t.TextDigest,
		Usage:        t.Usage,
		SessionID:    t.SessionID,
		MessagesRef:  t.MessagesRef,
		Messages:     t.Messages,
		GitRef:       t.GitRef,
		WrittenAt:    t.WrittenAt,
	}
	filter := turnKeyFilter(ctx, t.RunID, t.NodeID, t.LoopIter, t.TurnIndex)
	if _, err := s.runTurns.ReplaceOne(ctx, filter, doc, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("store/mongo: write turn %s/%s/%d/%d: %w", t.RunID, t.NodeID, t.LoopIter, t.TurnIndex, err)
	}
	return nil
}

// LoadTurn implements store.TurnStore: the turn at exact
// (node_id, loop_iter, turn_index), or ErrTurnNotFound.
func (s *Store) LoadTurn(ctx context.Context, runID, nodeID string, loopIter, turn int) (*store.TurnCheckpoint, error) {
	var doc runTurnDoc
	err := s.runTurns.FindOne(ctx, turnKeyFilter(ctx, runID, nodeID, loopIter, turn)).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("%w: run=%s node=%s iter=%d turn=%d", store.ErrTurnNotFound, runID, nodeID, loopIter, turn)
		}
		return nil, fmt.Errorf("store/mongo: load turn %s/%s/%d/%d: %w", runID, nodeID, loopIter, turn, err)
	}
	return doc.toCheckpoint(), nil
}

// ListTurns implements store.TurnStore: every turn for one
// (node_id, loop_iter) in ascending TurnIndex order. Empty slice (no
// error) when none exist. The inline messages blob is NOT surfaced —
// callers follow up with LoadTurnMessages, mirroring the fs reader.
func (s *Store) ListTurns(ctx context.Context, runID, nodeID string, loopIter int) ([]*store.TurnCheckpoint, error) {
	filter := withTenantFilter(ctx, bson.M{
		"run_id":    runID,
		"node_id":   nodeID,
		"loop_iter": loopIter,
	})
	opts := options.Find().
		SetSort(bson.D{{Key: "turn_index", Value: 1}}).
		SetProjection(bson.M{"messages": 0})
	cur, err := s.runTurns.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list turns %s/%s/%d: %w", runID, nodeID, loopIter, err)
	}
	defer cur.Close(ctx)

	var out []*store.TurnCheckpoint
	for cur.Next(ctx) {
		var doc runTurnDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode turn %s/%s/%d: %w", runID, nodeID, loopIter, err)
		}
		out = append(out, doc.toCheckpoint())
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: iterate turns %s/%s/%d: %w", runID, nodeID, loopIter, err)
	}
	return out, nil
}

// LatestTurn implements store.TurnStore: the highest-indexed turn for a
// node across all loop iterations — (highest loop_iter, highest
// turn_index within it) — or ErrTurnNotFound. Used by Fork to default
// turn_index to "the last completed turn".
func (s *Store) LatestTurn(ctx context.Context, runID, nodeID string) (*store.TurnCheckpoint, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID, "node_id": nodeID})
	opts := options.FindOne().
		SetSort(bson.D{{Key: "loop_iter", Value: -1}, {Key: "turn_index", Value: -1}}).
		SetProjection(bson.M{"messages": 0})
	var doc runTurnDoc
	if err := s.runTurns.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("%w: run=%s node=%s", store.ErrTurnNotFound, runID, nodeID)
		}
		return nil, fmt.Errorf("store/mongo: latest turn %s/%s: %w", runID, nodeID, err)
	}
	return doc.toCheckpoint(), nil
}

// LoadTurnAtIndex implements store.TurnStore: the turn at the given
// TurnIndex on the highest loop iteration that has one (a node inside a
// loop may only carry that turn on iter > 0). ErrTurnNotFound when no
// iteration has it.
func (s *Store) LoadTurnAtIndex(ctx context.Context, runID, nodeID string, turn int) (*store.TurnCheckpoint, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID, "node_id": nodeID, "turn_index": turn})
	opts := options.FindOne().
		SetSort(bson.D{{Key: "loop_iter", Value: -1}}).
		SetProjection(bson.M{"messages": 0})
	var doc runTurnDoc
	if err := s.runTurns.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("%w: run=%s node=%s turn=%d (any loop iter)", store.ErrTurnNotFound, runID, nodeID, turn)
		}
		return nil, fmt.Errorf("store/mongo: load turn at index %s/%s turn=%d: %w", runID, nodeID, turn, err)
	}
	return doc.toCheckpoint(), nil
}

// LoadTurnMessages implements store.TurnStore: the inline claw message
// blob for a turn, or ErrTurnNotFound when the turn is missing or carried
// no messages (a legacy turn or a non-claw backend).
func (s *Store) LoadTurnMessages(ctx context.Context, runID, nodeID string, loopIter, turn int) ([]byte, error) {
	opts := options.FindOne().SetProjection(bson.M{"messages": 1})
	var doc runTurnDoc
	err := s.runTurns.FindOne(ctx, turnKeyFilter(ctx, runID, nodeID, loopIter, turn), opts).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("%w: run=%s node=%s iter=%d turn=%d messages", store.ErrTurnNotFound, runID, nodeID, loopIter, turn)
		}
		return nil, fmt.Errorf("store/mongo: load turn messages %s/%s/%d/%d: %w", runID, nodeID, loopIter, turn, err)
	}
	if len(doc.Messages) == 0 {
		return nil, fmt.Errorf("%w: run=%s node=%s iter=%d turn=%d messages", store.ErrTurnNotFound, runID, nodeID, loopIter, turn)
	}
	// Guard: a well-formed messages blob is JSON (the claw []api.Message
	// slice). We don't parse it here, but reject a corrupt inline value
	// early rather than handing the Fork rehydration a garbage blob.
	if !json.Valid(doc.Messages) {
		return nil, fmt.Errorf("store/mongo: turn messages %s/%s/%d/%d are not valid JSON", runID, nodeID, loopIter, turn)
	}
	return doc.Messages, nil
}
