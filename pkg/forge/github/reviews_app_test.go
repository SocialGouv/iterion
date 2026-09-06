package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The review-thread reply gate reads a PR's review comments through the
// connection covering the repo — an App connection, by default — so the
// thread fetch must exist on the App client too, under the read profile the
// endpoint is gated on (pull_requests:read), issuing the PAT client's own
// requests (newest-first capped pages, handed back chronological).
func TestAppClientListPRReviewCommentsMintsThePullReadProfile(t *testing.T) {
	r := newPullMintRecorder(t)
	ctx := context.Background()
	fromPAT, err := r.patClient().ListPRReviewComments(ctx, "o/r", 7)
	if err != nil {
		t.Fatalf("PAT ListPRReviewComments: %v", err)
	}
	a := r.appClient(t)
	fromApp, err := a.ListPRReviewComments(ctx, "o/r", 7)
	if err != nil {
		t.Fatalf("App ListPRReviewComments: %v", err)
	}
	if !reflect.DeepEqual(fromApp, fromPAT) || len(fromApp) != 2 || fromApp[0].ID != 9001 {
		t.Fatalf("App = %+v, PAT = %+v: want the same chronological thread", fromApp, fromPAT)
	}
	mints, bearers, paths := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d, want exactly 1 (the PAT call mints nothing)", len(mints))
	}
	if !reflect.DeepEqual(mints[0], PRReviewCommentsInstallationPermissions()) || mints[0]["pull_requests"] != "read" || len(mints[0]) != 2 {
		t.Errorf("minted permissions = %v, want exactly pull_requests:read + metadata:read", mints[0])
	}
	if len(paths)%2 != 0 || len(paths) == 0 {
		t.Fatalf("paths = %v, want the App half to mirror the PAT half", paths)
	}
	half := len(paths) / 2
	for i := 0; i < half; i++ {
		if paths[i] != paths[half+i] {
			t.Errorf("request %d: PAT %q vs App %q", i, paths[i], paths[half+i])
		}
		if bearers[i] != "Bearer ghp_pat" || !strings.HasPrefix(bearers[half+i], "Bearer ghs_scoped_") {
			t.Errorf("bearers = %v, want the PAT then the minted installation token", bearers)
		}
	}
	// The profile IS the pull-list profile by key, so the two reads share
	// one cached token: a listing after the thread fetch mints nothing more.
	if _, err := a.ListPullRequests(ctx, "o/r", pullListOpts()); err != nil {
		t.Fatal(err)
	}
	if mints, _, _ := r.snapshot(); len(mints) != 1 {
		t.Errorf("mints = %d after a pull listing, want still 1: the thread fetch and the listing share a profile", len(mints))
	}
}
