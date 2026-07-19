package mongoutil

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// The Find*/ReplaceOne*/UpdateOne*/DeleteOne* helpers require a live
// *mongo.Collection and are exercised through the mongo-conformance CI
// job via their store callers; only the stateless helpers are covered
// here.

func TestIsIndexConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"IndexOptionsConflict 85", mongo.CommandError{Code: 85, Message: "index already exists with different options"}, true},
		{"IndexKeySpecsConflict 86", mongo.CommandError{Code: 86, Message: "key specs conflict"}, true},
		{"other command error", mongo.CommandError{Code: 11000, Message: "dup key"}, false},
		{"zero-code command error", mongo.CommandError{}, false},
		{"wrapped 85", fmt.Errorf("ensure schema: %w", mongo.CommandError{Code: 85}), true},
		{"wrapped 86", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", mongo.CommandError{Code: 86})), true},
		{"wrapped other", fmt.Errorf("ensure schema: %w", mongo.CommandError{Code: 26}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIndexConflict(tt.err); got != tt.want {
				t.Errorf("IsIndexConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsDuplicateKey(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"command error E11000", mongo.CommandError{Code: 11000, Message: "E11000 duplicate key error"}, true},
		{"command error 11001", mongo.CommandError{Code: 11001}, true},
		{"command error capped 12582", mongo.CommandError{Code: 12582}, true},
		{"mongos 16460 with E11000 message", mongo.CommandError{Code: 16460, Message: "insert failed: E11000 duplicate key"}, true},
		{"16460 without E11000 message", mongo.CommandError{Code: 16460, Message: "some other failure"}, false},
		{"unrelated command error", mongo.CommandError{Code: 85}, false},
		{"write exception E11000", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "E11000 duplicate key error"}}}, true},
		{"write exception other code", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 2, Message: "bad value"}}}, false},
		{"wrapped write exception", fmt.Errorf("insert org: %w", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000}}}), true},
		{"wrapped command error", fmt.Errorf("insert org: %w", mongo.CommandError{Code: 11000}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDuplicateKey(tt.err); got != tt.want {
				t.Errorf("IsDuplicateKey(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNormalizePage(t *testing.T) {
	tests := []struct {
		name         string
		offset       int
		limit        int
		defaultLimit int64
		wantSkip     int64
		wantTake     int64
	}{
		{"both positive", 10, 25, 50, 10, 25},
		{"zero offset zero limit", 0, 0, 50, 0, 50},
		{"negative offset clamped", -5, 20, 50, 0, 20},
		{"negative limit uses default", 3, -1, 50, 3, 50},
		{"both negative", -1, -1, 100, 0, 100},
		{"limit one kept", 0, 1, 50, 0, 1},
		{"large values pass through", 1 << 30, 1 << 30, 50, 1 << 30, 1 << 30},
		// Quirk: a non-positive defaultLimit is substituted verbatim —
		// NormalizePage does not guard the default itself.
		{"zero default with zero limit", 0, 0, 0, 0, 0},
		{"negative default with zero limit", 0, 0, -7, 0, -7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, take := NormalizePage(tt.offset, tt.limit, tt.defaultLimit)
			if skip != tt.wantSkip || take != tt.wantTake {
				t.Errorf("NormalizePage(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.offset, tt.limit, tt.defaultLimit, skip, take, tt.wantSkip, tt.wantTake)
			}
		})
	}
}
