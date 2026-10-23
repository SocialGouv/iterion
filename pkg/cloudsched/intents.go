package cloudsched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// LaunchIntent is the durable obligation of one nominal calendar slot. It is
// inserted BEFORE advancing the calendar, so a crash at either write leaves
// recoverable work. Completed rows retain the slot's idempotency key.
type LaunchIntent struct {
	ID        string       `bson:"_id"`
	Schedule  ScheduledBot `bson:"schedule"`
	NotBefore time.Time    `bson:"not_before"`
	ClaimID   string       `bson:"claim_id"`
	Done      bool         `bson:"done"`
	Prepared  bool         `bson:"prepared"`
}

const intentLease = 2 * time.Minute
const intentRetry = 15 * time.Second
const intentCollection = "schedule_launch_intents"

func intentID(sb ScheduledBot) string {
	return sb.ID + ":" + sb.NextFireAt.UTC().Format(time.RFC3339Nano)
}
func intentClaim() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *MemoryStore) PutIntent(_ context.Context, sb ScheduledBot) error {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	id := intentID(sb)
	if _, ok := s.intents[id]; ok {
		return nil
	}
	// Match the durable twin: the obligation owns its payload, never a caller's map.
	raw, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	var snapshot ScheduledBot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	s.intents[id] = LaunchIntent{ID: id, Schedule: snapshot}
	return nil
}
func (s *MemoryStore) ClaimIntents(ctx context.Context, now time.Time, limit int) ([]LaunchIntent, error) {
	return s.ClaimScheduleIntents(ctx, "", now, limit)
}
func (s *MemoryStore) ClaimScheduleIntents(_ context.Context, schedule string, now time.Time, limit int) ([]LaunchIntent, error) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	ids := []string{}
	for id, r := range s.intents {
		if !r.Done && !r.NotBefore.After(now) && (schedule == "" || r.Schedule.ID == schedule) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	rows := []LaunchIntent{}
	for _, id := range ids {
		r := s.intents[id]
		r.ClaimID = intentClaim()
		r.NotBefore = now.Add(intentLease)
		s.intents[id] = r
		rows = append(rows, r)
	}
	return rows, nil
}
func (s *MemoryStore) FinishIntent(_ context.Context, r LaunchIntent, hold bool, now time.Time) error {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	own, ok := s.intents[r.ID]
	if !ok || own.Done || own.ClaimID != r.ClaimID {
		return nil
	}
	own.Done = !hold
	own.NotBefore = now.Add(intentRetry)
	s.intents[r.ID] = own
	return nil
}
func (s *MongoStore) intentColl() *mongo.Collection {
	return s.kit.Coll().Database().Collection(intentCollection)
}
func (s *MongoStore) PutIntent(ctx context.Context, sb ScheduledBot) error {
	row := LaunchIntent{ID: intentID(sb), Schedule: sb}
	_, err := s.intentColl().UpdateOne(ctx, bson.M{"_id": row.ID}, bson.M{"$setOnInsert": row}, options.UpdateOne().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}
func (s *MongoStore) ClaimIntents(ctx context.Context, now time.Time, limit int) ([]LaunchIntent, error) {
	return s.ClaimScheduleIntents(ctx, "", now, limit)
}
func (s *MongoStore) ClaimScheduleIntents(ctx context.Context, schedule string, now time.Time, limit int) ([]LaunchIntent, error) {
	filter := bson.M{"done": false, "not_before": bson.M{"$lte": now}}
	if schedule != "" {
		filter["schedule._id"] = schedule
	}
	var rows []LaunchIntent
	for i := 0; i < limit; i++ {
		var row LaunchIntent
		err := s.intentColl().FindOneAndUpdate(ctx, filter,
			bson.M{"$set": bson.M{"claim_id": intentClaim(), "not_before": now.Add(intentLease)}},
			options.FindOneAndUpdate().SetSort(bson.D{{Key: "not_before", Value: 1}}).SetReturnDocument(options.After)).Decode(&row)
		if errors.Is(err, mongo.ErrNoDocuments) {
			break
		}
		if err != nil {
			return rows, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func (s *MongoStore) FinishIntent(ctx context.Context, r LaunchIntent, hold bool, now time.Time) error {
	_, err := s.intentColl().UpdateOne(ctx, bson.M{"_id": r.ID, "claim_id": r.ClaimID, "done": false}, bson.M{"$set": bson.M{"done": !hold, "not_before": now.Add(intentRetry)}})
	return err
}

// PrepareIntent stores the admitted guard's payload before launch. Maintenance
// retries must not run a potentially consuming guard a second time.
func (s *MemoryStore) PrepareIntent(_ context.Context, r LaunchIntent) (bool, error) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	own, ok := s.intents[r.ID]
	if !ok || own.ClaimID != r.ClaimID || own.Done {
		return false, nil
	}
	own.Schedule = r.Schedule
	own.Prepared = true
	s.intents[r.ID] = own
	return true, nil
}
func (s *MongoStore) PrepareIntent(ctx context.Context, r LaunchIntent) (bool, error) {
	res, err := s.intentColl().UpdateOne(ctx, bson.M{"_id": r.ID, "claim_id": r.ClaimID, "done": false}, bson.M{"$set": bson.M{"schedule": r.Schedule, "prepared": true}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}
