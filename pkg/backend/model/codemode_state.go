package model

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// CodemodeState is a handle to one bank. An engine binds it to the actual
// run store; a sandbox runner binds it to the negotiated launcher relay.
type CodemodeState interface {
	StoreState(context.Context, string, json.RawMessage) error
	LoadState(context.Context, string) (json.RawMessage, bool, error)
}

type codemodeStateKey struct{}

type codemodeStateScope struct{ bank CodemodeState }

// WithCodemodeState attaches state authority to execution. A nil handle
// masks any enclosing run's authority, rather than inheriting its bank.
func WithCodemodeState(ctx context.Context, bank CodemodeState) context.Context {
	return context.WithValue(ctx, codemodeStateKey{}, codemodeStateScope{bank})
}

func codemodeStateFrom(ctx context.Context) CodemodeState {
	scope, _ := ctx.Value(codemodeStateKey{}).(codemodeStateScope)
	return scope.bank
}

func (s *scriptToolServer) StoreState(ctx context.Context, key string, value json.RawMessage) error {
	if s.state == nil {
		return scripthost.ErrStateUnavailable
	}
	return s.state.StoreState(ctx, key, value)
}

func (s *scriptToolServer) LoadState(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if s.state == nil {
		return nil, false, scripthost.ErrStateUnavailable
	}
	return s.state.LoadState(ctx, key)
}

// codemodeStateRelay closes over the launcher authority, never an identity
// supplied by the runner. The worker's context carries the call deadline.
func codemodeStateRelay(ctx context.Context, enabled bool) func(context.Context, delegate.StateRequest) (delegate.StateReply, error) {
	bank := codemodeStateFrom(ctx)
	if !enabled || bank == nil {
		return nil
	}
	return func(ctx context.Context, req delegate.StateRequest) (delegate.StateReply, error) {
		var reply delegate.StateReply
		var err error
		switch req.Op {
		case delegate.StateStore:
			err = bank.StoreState(ctx, req.Key, req.Value)
		case delegate.StateLoad:
			reply.Value, reply.Found, err = bank.LoadState(ctx, req.Key)
		default:
			return delegate.StateReply{}, scripthost.ErrStateInvalid
		}
		switch {
		case err == nil:
			reply.Code = delegate.StateOK
			return reply, nil
		case errors.Is(err, scripthost.ErrStateInvalid):
			return delegate.StateReply{Code: delegate.StateInvalid}, nil
		case errors.Is(err, scripthost.ErrStateQuota):
			return delegate.StateReply{Code: delegate.StateQuota}, nil
		case errors.Is(err, scripthost.ErrStateUnavailable):
			return delegate.StateReply{Code: delegate.StateUnavailable}, nil
		default:
			return delegate.StateReply{}, errors.New("model: codemode state backend failed")
		}
	}
}
