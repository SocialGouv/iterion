package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

const gatewayAliveRunsPath = "/api/admin/llm/gateway-alive-runs"

var (
	subjA = store.GatewayCapacitySubject("a.example")
	subjB = store.GatewayCapacitySubject("b.example")
	subjY = store.GatewayCapacitySubject("y.example")
)

// keysOn maps the test fingerprints to the subject of the gateway their row
// names: fp-a on a.example, fp-b on b.example, fp-y on y.example, and fp-nohost
// on a row whose URL names no host.
var keysOn = map[string]string{"fp-a": subjA, "fp-b": subjB, "fp-y": subjY, "fp-nohost": ""}

func aliveRun(id string, fps []string, subjects ...string) store.Run {
	return store.Run{ID: id, BotID: "bot-" + id, Status: store.RunStatusRunning, CredFingerprints: fps, CapacitySubjects: subjects}
}

func subbotChild(r store.Run, parent string) store.Run {
	r.ParentRunID, r.ParentNodeID = parent, "review"
	return r
}

// reportBothOrders is the report over runs in their order and reversed; the
// two must agree.
func reportBothOrders(t *testing.T, runs []store.Run, subjectOf map[string]string) gatewayAliveRunsView {
	t.Helper()
	listed := gatewayAliveRunsReport(slices.Clone(runs), subjectOf)
	reversed := slices.Clone(runs)
	slices.Reverse(reversed)
	if again := gatewayAliveRunsReport(reversed, subjectOf); !reflect.DeepEqual(listed, again) {
		t.Fatalf("the report depends on the listing's order:\nlisted   %+v\nreversed %+v", listed, again)
	}
	return listed
}

func hostRow(view gatewayAliveRunsView, host string) (gatewayHostAliveRuns, bool) {
	for _, h := range view.Hosts {
		if h.Host == host {
			return h, true
		}
	}
	return gatewayHostAliveRuns{}, false
}

func wantHost(t *testing.T, view gatewayAliveRunsView, host string, trees, runs, unnamed int) gatewayHostAliveRuns {
	t.Helper()
	h, ok := hostRow(view, host)
	if !ok {
		t.Fatalf("no row for %s in %+v", host, view.Hosts)
	}
	if h.AliveTrees != trees || h.AliveRuns != runs || h.UnnamedTrees != unnamed {
		t.Fatalf("%s: alive_trees=%d alive_runs=%d unnamed_trees=%d, want %d/%d/%d (view %+v)", host, h.AliveTrees, h.AliveRuns, h.UnnamedTrees, trees, runs, unnamed, view)
	}
	return h
}

func TestGatewayAliveRunsReport(t *testing.T) {
	t.Run("(i) a run stamped before the subjects is unnamed on its key's host", func(t *testing.T) {
		v := reportBothOrders(t, []store.Run{aliveRun("pre", []string{"fp-a"})}, keysOn)
		wantHost(t, v, "a.example", 0, 0, 1)
	})
	t.Run("(ii) a run naming another host than its key's is unnamed on its key's", func(t *testing.T) {
		v := reportBothOrders(t, []store.Run{aliveRun("moved", []string{"fp-b"}, subjA)}, keysOn)
		wantHost(t, v, "a.example", 1, 1, 0)
		wantHost(t, v, "b.example", 0, 0, 1)
	})
	t.Run("(iii) a run naming both hosts its keys dial is unnamed on neither", func(t *testing.T) {
		v := reportBothOrders(t, []store.Run{aliveRun("both", []string{"fp-a", "fp-b"}, subjA, subjB)}, keysOn)
		wantHost(t, v, "a.example", 1, 1, 0)
		wantHost(t, v, "b.example", 1, 1, 0)
	})
	t.Run("(iv) an in-process child is judged with its stamped parent", func(t *testing.T) {
		child := subbotChild(store.Run{ID: "child", Status: store.RunStatusRunning, ContentBoundary: &store.RunContentBoundary{KeyID: "key-gw"}}, "parent")
		v := reportBothOrders(t, []store.Run{aliveRun("parent", []string{"fp-a"}, subjA), child}, keysOn)
		wantHost(t, v, "a.example", 1, 1, 0)
		if v.UnresolvedTrees != 0 {
			t.Fatalf("unresolved_trees = %d, want 0 — the child's tree names its host", v.UnresolvedTrees)
		}
	})
	t.Run("(v) a child resumed on its own is one tree with its parent", func(t *testing.T) {
		v := reportBothOrders(t, []store.Run{aliveRun("parent", []string{"fp-a"}, subjA), subbotChild(aliveRun("child", []string{"fp-a"}, subjA), "parent")}, keysOn)
		h := wantHost(t, v, "a.example", 1, 2, 0)
		if !reflect.DeepEqual(h.Bots, []gatewayBotAliveTrees{{BotID: "bot-parent", AliveTrees: 1}}) {
			t.Fatalf("bots = %+v, want the tree counted once under its root's bot", h.Bots)
		}
	})
	t.Run("(vi) a fork is a tree of its own", func(t *testing.T) {
		fork := aliveRun("fork", []string{"fp-a"}, subjA)
		fork.ParentRunID = "origin"
		v := reportBothOrders(t, []store.Run{aliveRun("origin", []string{"fp-a"}, subjA), fork}, keysOn)
		wantHost(t, v, "a.example", 2, 2, 0)
	})
	t.Run("(vii) the host is the key row's, never the content boundary's", func(t *testing.T) {
		r := aliveRun("frozen", []string{"fp-y"})
		r.ContentBoundary = &store.RunContentBoundary{KeyID: "key-gw", GatewayHost: "x.example"}
		v := reportBothOrders(t, []store.Run{r}, keysOn)
		wantHost(t, v, "y.example", 0, 0, 1)
		if h, ok := hostRow(v, "x.example"); ok {
			t.Fatalf("x.example reported %+v — the boundary's frozen host is no host the run dials", h)
		}
		if v.UnresolvedTrees != 0 {
			t.Fatalf("unresolved_trees = %d, want 0 — its key's row names the host it is on", v.UnresolvedTrees)
		}
	})
	t.Run("a run the listing holds in transit counts on its subject, whatever the reader's clock", func(t *testing.T) {
		// The listing decided at its own instant that the run held its
		// places; the hold has lapsed since by this reader's clock. Every
		// reading groups the one listing by stamp — a reader that judged the
		// hold again would count what the ledger's count did not.
		lapsed := time.Now().Add(-time.Second)
		r := aliveRun("transit", []string{"fp-a"}, subjA)
		r.Status, r.ContinuationState, r.ContinuationUntil = store.RunStatusFailedResumable, store.ContinuationRedeliveryPending, &lapsed
		v := reportBothOrders(t, []store.Run{r}, keysOn)
		h := wantHost(t, v, "a.example", 1, 1, 0)
		if !reflect.DeepEqual(h.Bots, []gatewayBotAliveTrees{{BotID: "bot-transit", AliveTrees: 1}}) {
			t.Fatalf("bots = %+v, want the transit run's tree under its bot", h.Bots)
		}
	})
	t.Run("(viii) a rotated key with no subject is unresolved", func(t *testing.T) {
		r := aliveRun("rotated", []string{"fp-gone"})
		r.ContentBoundary = &store.RunContentBoundary{KeyID: "key-gone"}
		v := reportBothOrders(t, []store.Run{r}, keysOn)
		if v.UnresolvedTrees != 1 || len(v.Hosts) != 0 {
			t.Fatalf("unresolved_trees=%d hosts=%+v, want 1 and none", v.UnresolvedTrees, v.Hosts)
		}
	})
	t.Run("a rotated key under a stamped subject is on its subject's host, not unresolved", func(t *testing.T) {
		r := aliveRun("rotated-named", []string{"fp-gone"}, subjA)
		r.ContentBoundary = &store.RunContentBoundary{KeyID: "key-gone"}
		v := reportBothOrders(t, []store.Run{r}, keysOn)
		wantHost(t, v, "a.example", 1, 1, 0)
		if v.UnresolvedTrees != 0 {
			t.Fatalf("unresolved_trees = %d, want 0 — its subject names the host", v.UnresolvedTrees)
		}
	})
	t.Run("a run that never sealed a gateway key is not unresolved", func(t *testing.T) {
		vendorOnly := aliveRun("vendor", []string{"fp-vendor"})
		unstamped := aliveRun("unstamped", nil)
		v := reportBothOrders(t, []store.Run{vendorOnly, unstamped}, keysOn)
		if v.UnresolvedTrees != 0 || len(v.Hosts) != 0 {
			t.Fatalf("unresolved_trees=%d hosts=%+v, want 0 and none — no content boundary records a sealed gateway key", v.UnresolvedTrees, v.Hosts)
		}
	})
	t.Run("a run whose latest stamp read the subjects and names none left the gateway", func(t *testing.T) {
		// The content boundary keeps the key id its launch froze; the
		// stamp written by the resume that sealed no gateway key says so.
		cleared := aliveRun("cleared", nil)
		onVendor := aliveRun("on-vendor", []string{"fp-vendor"})
		for _, r := range []*store.Run{&cleared, &onVendor} {
			store.RunCredStamp{Fingerprints: r.CredFingerprints, CapacityStamped: true}.Apply(r)
			r.ContentBoundary = &store.RunContentBoundary{KeyID: "key-gw", GatewayHost: "a.example"}
		}
		child := subbotChild(store.Run{ID: "cleared-child", Status: store.RunStatusRunning, ContentBoundary: &store.RunContentBoundary{KeyID: "key-gw"}}, "cleared")
		v := reportBothOrders(t, []store.Run{cleared, onVendor, child}, keysOn)
		if v.UnresolvedTrees != 0 || len(v.Hosts) != 0 {
			t.Fatalf("unresolved_trees=%d hosts=%+v, want 0 and none — a stamp that read the subjects is exact, the launch's key id is history", v.UnresolvedTrees, v.Hosts)
		}
	})
	t.Run("a stamp an older replica rewrote no longer says its subjects were read", func(t *testing.T) {
		// A T2 resolution stamped the gateway's subject and marked it read;
		// a T1 replica resumed the run during the rolling deploy — the same
		// key, its subjects cleared, the marker it does not know left in
		// place — and the key's row was then rotated away. The marker no
		// longer matches the lists it qualified, so the launch's key id is
		// the only witness left: the tree is unresolved, never invisible.
		for _, c := range []struct {
			name string
			fps  []string
		}{{"same key", []string{"fp-gone"}}, {"another key", []string{"fp-gone-v2"}}} {
			r := aliveRun("t1-resumed", []string{"fp-gone"})
			store.RunCredStamp{Fingerprints: r.CredFingerprints, CapacitySubjects: []string{subjA}, CapacityStamped: true}.Apply(&r)
			r.CredFingerprints, r.CapacitySubjects = c.fps, nil
			r.ContentBoundary = &store.RunContentBoundary{KeyID: "key-gone", GatewayHost: "a.example"}
			v := reportBothOrders(t, []store.Run{r}, keysOn)
			if v.UnresolvedTrees != 1 || len(v.Hosts) != 0 {
				t.Fatalf("%s: unresolved_trees=%d hosts=%+v, want 1 and none — a run alive on a gateway the reading cannot name must be counted somewhere", c.name, v.UnresolvedTrees, v.Hosts)
			}
		}
	})
	t.Run("(ix) a key whose row names no host is unresolved", func(t *testing.T) {
		v := reportBothOrders(t, []store.Run{aliveRun("nohost", []string{"fp-nohost"})}, keysOn)
		if v.UnresolvedTrees != 1 || len(v.Hosts) != 0 {
			t.Fatalf("unresolved_trees=%d hosts=%+v, want 1 and none", v.UnresolvedTrees, v.Hosts)
		}
	})
}

// opaqueRunStore hides every optional capability of the store it wraps.
type opaqueRunStore struct{ store.RunStore }

// gatewayAliveRunsServer boots the production route stack over rs and keys.
func gatewayAliveRunsServer(t *testing.T, rs store.RunStore, keys secrets.ApiKeyStore) (srv interface{ Close() }, do func(token string) (int, []byte), adminTok, userTok string) {
	t.Helper()
	_, _, _, _, hs, adminTok, userTok := newAdminLLMServerWith(t, func(c *Config) {
		c.Store = rs
		if keys != nil {
			c.ApiKeys = keys
		}
	})
	return hs, func(token string) (int, []byte) { return llmDo(t, hs, http.MethodGet, gatewayAliveRunsPath, token, "") }, adminTok, userTok
}

func TestGatewayAliveRunsRoute_SuperAdminOnly(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, do, _, userTok := gatewayAliveRunsServer(t, rs, secrets.NewMemoryApiKeyStore())
	if code, body := do(""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s, want 401", code, body)
	}
	if code, body := do(userTok); code != http.StatusForbidden {
		t.Fatalf("team admin: %d %s, want 403 — the reading spans every team", code, body)
	}
}

// A store that cannot answer is said, never an empty success.
func TestGatewayAliveRunsRoute_AStoreWithoutTheReadingAnswers501(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, do, adminTok, _ := gatewayAliveRunsServer(t, opaqueRunStore{rs}, secrets.NewMemoryApiKeyStore())
	if code, body := do(adminTok); code != http.StatusNotImplemented || !strings.Contains(string(body), gatewayAliveRunsNoListingCode) {
		t.Fatalf("run store without the listing: %d %s, want 501 with error_code %s", code, body, gatewayAliveRunsNoListingCode)
	}
	_, do, adminTok, _ = gatewayAliveRunsServer(t, rs, nil)
	if code, body := do(adminTok); code != http.StatusNotImplemented || !strings.Contains(string(body), gatewayAliveRunsNoKeyLookupCode) {
		t.Fatalf("key store without the lookup: %d %s, want 501 with error_code %s", code, body, gatewayAliveRunsNoKeyLookupCode)
	}
}

// End to end through the stores: a run stamped by the publisher's fold names
// the subject the route maps its key's row to, whatever the row's spelling.
func TestGatewayAliveRunsRoute_ReadsTheStoresThroughTheSharedFold(t *testing.T) {
	const legacyURL = "https://GW.Example.com./"
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	ctx := store.WithTenant(context.Background(), "team1")
	if err := keys.Create(ctx, secrets.ApiKey{ID: "k-gw", ScopeTeamID: "team1", Provider: secrets.ProviderOpenAICompatible, Name: "legacy", BaseURL: legacyURL, Fingerprint: "fp-legacy"}); err != nil {
		t.Fatal(err)
	}
	subject := cloudpublisher.GatewayCapacitySubjectOf(legacyURL)
	for id, stamp := range map[string]store.RunCredStamp{
		"stamped": {Fingerprints: []string{"fp-legacy"}, CapacitySubjects: []string{subject}},
		"pre-t2":  {Fingerprints: []string{"fp-legacy"}},
	} {
		r, err := rs.CreateRun(ctx, id, "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Status = store.RunStatusRunning
		r.BotID = "review-pr"
		stamp.Apply(r)
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_, do, adminTok, _ := gatewayAliveRunsServer(t, rs, keys)
	code, body := do(adminTok)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", gatewayAliveRunsPath, code, body)
	}
	var view gatewayAliveRunsView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if view.ReadAt.IsZero() || len(view.Hosts) != 1 {
		t.Fatalf("view = %s, want one host and a read_at", body)
	}
	h := view.Hosts[0]
	if h.Host != "gw.example.com" || h.Subject != subject || h.AliveTrees != 1 || h.AliveRuns != 1 || h.UnnamedTrees != 1 {
		t.Fatalf("host row = %+v, want gw.example.com: 1 tree and 1 run named, 1 tree unnamed (the pre-T2 run) — the stamped run's subject is the one its row maps to", h)
	}
	if !reflect.DeepEqual(h.Bots, []gatewayBotAliveTrees{{BotID: "review-pr", AliveTrees: 1}}) {
		t.Fatalf("bots = %+v", h.Bots)
	}
}
