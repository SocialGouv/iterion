package main

// cloud_refresh_loop.go carries NO build tag, like cloud_client.go: the
// background refresh loop is plain time over the token jar, so its cadence
// and the way it classifies a refresh error are unit-tested without the
// Wails/webkit toolchain. cloud.go (//go:build desktop) runs one per cloud
// connection and hands it the event emitter.

import (
	"context"
	"errors"
	"log"
	"time"
)

// cloudRefreshLoop keeps a cloud connection's access token fresh. It
// refreshes cloudRefreshSkew before the token expires, and sooner when a
// refresh the tunnel started was not served (cloudTokenJar.kickRefresh): a
// token the server may have rotated has to be presented again while that
// rotation's successor is still redeliverable, not at the next expiry-based
// tick, which an access token that lapsed early — a machine resumed from
// sleep, a signing-key rotation, a clock behind the server's — puts minutes
// away. Two presentations of this loop's are never closer than
// cloudRefreshRetryFloor, whatever wakes it.
//
// A refresh the cloud REFUSES (cloudRefreshRefused) ends the session: it
// calls onExpired — the shell then prompts for a new sign-in — and returns.
// Anything else is a failure the next attempt may be served.
//
// A connection whose sign-out the cloud has not confirmed
// (cloudTokenJar.SignOutPending) is not renewed at all: this loop presents
// that sign-out instead, on its own back-off, and returns once the cloud
// confirms it. It emits no cloud:auth-expired for it — the operator ended
// that session on purpose, and the shell has nothing to recover.
//
// Sign-outs HELD for sessions this connection replaced (a sign-in that could
// not confirm one: cloudTokenJar.prepareForSignIn) are presented on every
// pass beside the renewals, and the loop outlives its own exits for them
// (drainHeldSignOuts): those tokens are the only thing that can end sessions
// the cloud still holds live.
func cloudRefreshLoop(ctx context.Context, jar *cloudTokenJar, onExpired func(connID string)) {
	var last time.Time
	signOuts := 0
	for {
		wait := time.Until(jar.Expiry().Add(-cloudRefreshSkew))
		if wait < cloudRefreshRetryFloor {
			wait = cloudRefreshRetryFloor
		}
		if jar.SignOutPending() {
			wait = cloudBackoff(signOuts, cloudSignOutBackoffCap)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-jar.woken():
			timer.Stop()
			if since := time.Since(last); !last.IsZero() && since < cloudRefreshRetryFloor {
				if !waitFor(ctx, cloudRefreshRetryFloor-since) {
					return
				}
			}
		}
		last = time.Now()
		if err := jar.presentHeldSignOuts(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("desktop: the cloud has not confirmed a sign-out %s holds for a session it replaced: %v (presenting it again)", jar.connID, err)
		}
		if jar.SignOutPending() {
			if err := jar.retrySignOut(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				signOuts++
				log.Printf("desktop: the cloud has not confirmed the sign-out of %s: %v (presenting it again)", jar.connID, err)
				continue
			}
			log.Printf("desktop: the cloud confirmed the pending sign-out of %s", jar.connID)
			drainHeldSignOuts(ctx, jar)
			return
		}
		if err := jar.refreshNow(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if cloudRefreshRefused(err) {
				if jar.SignOutPending() {
					// A sign-out held while this renewal was in flight: its
					// answer is the ended session's own successor, dropped
					// (seedAt), and the sign-out is what the next pass
					// presents. Not an expiry: nothing is over for the shell
					// to recover.
					continue
				}
				if jar.LoggedIn() {
					// The refusal named a token this jar no longer holds: a
					// sign-in or a rotation replaced it while the refusal was
					// in flight (clearIfStillHolds kept the jar). It says
					// nothing about the session the jar holds now — renew on
					// instead of asking the shell for a sign-in (F13).
					continue
				}
				log.Printf("desktop: cloud refresh refused for %s: %v", jar.connID, err)
				if onExpired != nil {
					onExpired(jar.connID)
				}
				drainHeldSignOuts(ctx, jar)
				return
			}
			log.Printf("desktop: cloud refresh transient error for %s: %v (will retry)", jar.connID, err)
			// Expiry is unchanged (still past), so the next iteration floors
			// to cloudRefreshRetryFloor and retries.
		}
	}
}

// drainHeldSignOuts presents the sign-outs this connection holds for sessions
// it replaced until the cloud confirms them all, on the sign-out back-off. It
// runs where cloudRefreshLoop would otherwise return — the live session over,
// or its own sign-out confirmed — because nothing else is left to present
// them, and returns at once when none is held.
func drainHeldSignOuts(ctx context.Context, jar *cloudTokenJar) {
	for attempts := 0; ; attempts++ {
		err := jar.presentHeldSignOuts(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Printf("desktop: the cloud has not confirmed a sign-out %s holds for a session it replaced: %v (presenting it again)", jar.connID, err)
		if !waitFor(ctx, cloudBackoff(attempts, cloudSignOutBackoffCap)) {
			return
		}
	}
}

// waitFor waits d, and reports false when ctx ended first.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// logCloudRehydrate reports what a connection's startup refresh means for
// the pane about to mount: a refusal leaves the entry with no credential, so
// its first request is answered 401 and the shell asks for a new sign-in;
// anything else is a failure the background loop retries.
func logCloudRehydrate(connID string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, errCloudSignOutPending):
		log.Printf("desktop: the stored cloud session for %s carries a sign-out the cloud has not confirmed: nothing is presented, and the background loop presents the sign-out until it is", connID)
	case cloudRefreshRefused(err):
		log.Printf("desktop: the stored cloud session for %s is over (%v): the pane's requests are answered 401 and the shell asks for a new sign-in", connID, err)
	default:
		log.Printf("desktop: cloud rehydrate refresh for %s failed: %v (the background loop retries)", connID, err)
	}
}
