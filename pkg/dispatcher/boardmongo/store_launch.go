package boardmongo

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The admission loop's atomic launch claim (native.LaunchClaimer), Mongo
// twin of native.Store.ClaimForLaunch.

// Compile-time assertion: the cloud board carries the launch CAS. The
// interface is optional by design (native.AsLaunchClaimer), which is
// exactly why this pin exists — a backend that loses the method degrades
// the studio's pipeline admission to a best-effort SetState, a second
// launch authority that never reads the claim.
var _ native.LaunchClaimer = (*Store)(nil)

// ClaimForLaunch moves a Ready ticket to in_progress for the caller that
// wins it, reporting whether THIS caller won. ONE conditional write
// carries the whole precondition: the card is still in StateReady AND
// carries no claim. The claim half is what makes it safe under the claim
// lease: the dispatcher wins a card with the CLAIM and moves it to
// in_progress afterwards, off the actor, so a claimed card can legally
// sit in Ready while its run is already launching — a launcher reading
// the state alone starts a second run on it. "Unclaimed" includes an
// ABSENT claim field, like Claim: a document that lost the field to an
// out-of-band write must stay launchable.
//
// Not won is (nil, false, nil) — the FS twin's answer for both "not
// ready" and "held"; a missing card is ErrNotFound.
func (s *Store) ClaimForLaunch(id string) (*native.Issue, bool, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	res := s.issues.FindOneAndUpdate(ctx,
		bson.M{
			"_id":         id,
			"tenant_id":   s.tenant,
			"issue.state": native.StateReady,
			"issue.claim": bson.M{"$in": bson.A{"", nil}},
		},
		bson.M{"$set": stateSetAt(native.StateInProgress, "", time.Now().UTC())},
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, false, fmt.Errorf("boardmongo: claim for launch: %w", res.Err())
		}
		// Zero match: not ready, or somebody holds it — either way not
		// ours. A missing card must still surface as ErrNotFound.
		if _, gerr := s.get(ctx, id); gerr != nil {
			return nil, false, gerr
		}
		return nil, false, nil
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("boardmongo: claim for launch decode: %w", err)
	}
	if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id,
		Payload: map[string]any{"from": native.StateReady, "to": native.StateInProgress}}); err != nil {
		return nil, false, err
	}
	return &doc.Issue, true, nil
}
