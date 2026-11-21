package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

type runnerCodemodeState struct{ d *proxyDispatcher }

func (s runnerCodemodeState) StoreState(ctx context.Context, key string, value json.RawMessage) error {
	_, err := s.call(ctx, delegate.StateRequest{Op: delegate.StateStore, Key: key, Value: value})
	return err
}

func (s runnerCodemodeState) LoadState(ctx context.Context, key string) (json.RawMessage, bool, error) {
	reply, err := s.call(ctx, delegate.StateRequest{Op: delegate.StateLoad, Key: key})
	return reply.Value, reply.Found, err
}

func (s runnerCodemodeState) call(ctx context.Context, req delegate.StateRequest) (delegate.StateReply, error) {
	ctx, cancel := context.WithTimeout(ctx, delegate.StateRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return delegate.StateReply{}, err
	}
	deadline, _ := ctx.Deadline()
	req.DeadlineUnixNano = deadline.UnixNano()
	id := "codemode-state-" + strconv.FormatUint(s.d.nextID.Add(1), 10)
	env, err := delegate.NewStateRequest(id, req)
	if err != nil {
		return delegate.StateReply{}, err
	}
	env, err = s.d.call(ctx, env)
	if err != nil {
		if req.Op == delegate.StateStore {
			return delegate.StateReply{}, scripthost.ErrStateWriteUnconfirmed
		}
		return delegate.StateReply{}, errors.New("runner: codemode state request did not complete")
	}
	reply, err := delegate.DecodeStateReply(env, id, req.Op)
	if err != nil {
		if req.Op == delegate.StateStore {
			return delegate.StateReply{}, scripthost.ErrStateWriteUnconfirmed
		}
		return delegate.StateReply{}, err
	}
	switch reply.Code {
	case delegate.StateUnavailable:
		return delegate.StateReply{}, scripthost.ErrStateUnavailable
	case delegate.StateQuota:
		return delegate.StateReply{}, scripthost.ErrStateQuota
	case delegate.StateInvalid:
		return delegate.StateReply{}, scripthost.ErrStateInvalid
	default:
		return reply, nil
	}
}
