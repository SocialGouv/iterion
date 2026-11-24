package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The two classes a leased sweep's caller tells apart: a refusal that came
// BACK from the forge (proof this node reaches it) and a per-node reach
// failure (the class a peer on another node may well not share).
var (
	sweepRefusal     = &StatusError{Provider: ProviderGitLab, Op: "refresh token", Code: 422}
	sweepUpstream502 = &StatusError{Provider: ProviderGitLab, Op: "refresh token", Code: 502}
)

func sweepUnreachable() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://gitlab.example.com/oauth/token",
		Err: &net.DNSError{Err: "no such host", Name: "gitlab.example.com", IsNotFound: true},
	}
}

// orderedConnections lists the due connections in a FIXED order, which is the
// variable of these tests: the memory store answers from a map, so the order
// a real query returns is neither stable nor the test's to assume.
type orderedConnections struct {
	connection.ForgeStore
	due []Connection
}

func (s orderedConnections) ExpiringBefore(context.Context, time.Time) ([]Connection, error) {
	return s.due, nil
}

// seedRefreshableConn creates one connection the refresh worker will take up:
// active, refreshable, its payload sealed so refreshOne reaches the
// refresher, and expiring inside the lead window.
func seedRefreshableConn(t *testing.T, sealer secrets.Sealer, store connection.ForgeStore, id string, expiresAt time.Time) Connection {
	t.Helper()
	blob, err := connection.SealForgeOAuthTokens(sealer, id, "old-"+id, "refresh-"+id, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	exp := expiresAt
	c := Connection{
		ID: id, TenantID: "t1", Provider: ProviderGitLab, Kind: KindOAuthApp,
		Status: StatusActive, SealedPayload: blob, AccessTokenExpiresAt: &exp,
	}
	if err := store.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The refresh sweep is elected (#2685): its caller reads the CLASS of the
// error to decide whether this replica can reach the forge at all, so a sweep
// that kept only the FIRST of its failures handed over a verdict that
// depended on the order its own query listed the connections in. One refusal
// listed before one unreachable connection read "the far side answered",
// which clears the run of unreachable passes a handover counts.
//
// Both orders here, and the aggregate is identical in both: every cause
// travels, ordered by the connection it belongs to.
func TestRefreshWorker_RunOnceHandsEveryCauseToItsCallerInAStableOrder(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	conns := connection.NewMemoryForgeStore()
	refused := seedRefreshableConn(t, sealer, conns, "conn-a-refused", now.Add(2*time.Minute))
	unreachable := seedRefreshableConn(t, sealer, conns, "conn-b-unreachable", now.Add(2*time.Minute))
	failures := map[string]error{
		refused.ID:     sweepRefusal,
		unreachable.ID: sweepUnreachable(),
	}

	aggregates := map[string]string{}
	for _, tc := range []struct {
		name string
		due  []Connection
	}{
		{"the refusal listed first", []Connection{refused, unreachable}},
		{"the unreachable connection listed first", []Connection{unreachable, refused}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &RefreshWorker{
				Connections: orderedConnections{ForgeStore: conns, due: tc.due},
				Secrets:     secrets.NewMemoryGenericSecretStore(),
				Sealer:      sealer,
				Now:         func() time.Time { return now },
				RefresherFor: func(c Connection) TokenRefresher {
					return fakeRefresher{err: failures[c.ID]}
				},
			}
			refreshedCount, err := w.RunOnce(context.Background())
			if refreshedCount != 0 {
				t.Fatalf("refreshed = %d, want 0: both connections failed", refreshedCount)
			}
			if err == nil {
				t.Fatal("RunOnce reported no failure for two failing connections")
			}
			// Every cause travels with the result, not just the first.
			if !errors.Is(err, sweepRefusal) {
				t.Errorf("the refusal did not reach the caller: %v", err)
			}
			var dns *net.DNSError
			if !errors.As(err, &dns) {
				t.Errorf("the unreachable connection did not reach the caller: %v", err)
			}
			// And the verdict its caller reads is the same in both orders.
			if !lease.TransportFailure(err) {
				t.Errorf("lease.TransportFailure = false for a sweep that met an unreachable connection: %v", err)
			}
			aggregates[tc.name] = err.Error()
		})
	}
	var texts []string
	for _, text := range aggregates {
		texts = append(texts, text)
	}
	if len(texts) != 2 {
		t.Fatalf("only %d of the 2 orders produced an aggregate, so the claim that both read the same was never tested: %q", len(texts), texts)
	}
	if texts[0] != texts[1] {
		t.Errorf("the aggregate depends on the order the query listed the connections in:\n%q\nvs\n%q", texts[0], texts[1])
	}
}

// What a net KNOWS about its own far side reaches the classifier: an upstream
// 5xx is a gateway between this node and the forge answering for it, so the
// call never arrived. Without the declaration the same sweep read
// VerdictRefused and the sick holder kept its term.
func TestRefreshWorker_AnUpstream5xxIsAReachFailureForTheHandover(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	conns := connection.NewMemoryForgeStore()
	gatewayed := seedRefreshableConn(t, sealer, conns, "conn-502", now.Add(2*time.Minute))
	w := &RefreshWorker{
		Connections:  conns,
		Secrets:      secrets.NewMemoryGenericSecretStore(),
		Sealer:       sealer,
		Now:          func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{err: sweepUpstream502} },
	}
	refreshedCount, err := w.RunOnce(context.Background())
	if refreshedCount != 0 || err == nil {
		t.Fatalf("RunOnce = (%d, %v), want (0, the gateway's answer)", refreshedCount, err)
	}
	if !lease.TransportFailure(err) {
		t.Fatalf("a %s sweep reads refused: %v — the far side never answered", gatewayed.ID, err)
	}
	var pass lease.Pass
	pass.Attempted(1, 0, err)
	if got := pass.Verdict(); got != lease.VerdictUnreachable {
		t.Fatalf("the pass reads %v, want unreachable: a cleared run keeps the sick holder's term", got)
	}
}

// The declaration is read for every status, not just the one a test picked,
// and it says exactly what Upstream5xx says — one answer, not a second list
// that drifts from it.
func TestStatusError_DeclaresTheReachFailureClassItsCallersAlreadyRead(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{422, false}, {429, false}, {403, false}, {404, false},
		{500, true}, {502, true}, {503, true}, {504, true},
	} {
		se := &StatusError{Provider: ProviderGitHub, Op: "mint installation token", Code: tc.code}
		if got := se.LeaseUnreachable(); got != tc.want {
			t.Errorf("(&StatusError{Code: %d}).LeaseUnreachable() = %v, want %v", tc.code, got, tc.want)
		}
		if se.LeaseUnreachable() != se.Upstream5xx() {
			t.Errorf("HTTP %d: the declaration and Upstream5xx disagree — one of them is a second list", tc.code)
		}
		if got := lease.TransportFailure(se); got != tc.want {
			t.Errorf("lease.TransportFailure(HTTP %d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// orderedIntegrations lists the due project bots in a FIXED order, for the
// same reason orderedConnections does.
type orderedIntegrations struct {
	RepoIntegrationStore
	due []RepoIntegration
}

func (s orderedIntegrations) ListProjectBotsExpiringBefore(context.Context, time.Time) ([]RepoIntegration, error) {
	return s.due, nil
}

// failingSecretReads fails ONE named secret read per integration, which is
// how a rotation meets a per-item failure before it ever asks the forge.
type failingSecretReads struct {
	secrets.GenericSecretStore
	byID map[string]error
}

func (s failingSecretReads) Get(ctx context.Context, id string) (secrets.GenericSecret, error) {
	if err := s.byID[id]; err != nil {
		return secrets.GenericSecret{}, err
	}
	return s.GenericSecretStore.Get(ctx, id)
}

// The project-bot rotation is the GitLab twin of the refresh sweep, on the
// same ticker and the same elected term, so it owes the same answer: every
// cause, ordered by the integration it belongs to, and one verdict for both
// orders.
func TestRotateDueProjectBots_HandsEveryCauseToItsCallerInAStableOrder(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	refused := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	unreachable := age(t, o, provisionProjectBot(t, o, conn, "group/web"), tenDays, threeDays)
	o.Secrets = failingSecretReads{GenericSecretStore: o.Secrets, byID: map[string]error{
		refused.ManagedSecretID:     sweepRefusal,
		unreachable.ManagedSecretID: sweepUnreachable(),
	}}
	integrations := o.Integrations

	aggregates := map[string]string{}
	for _, tc := range []struct {
		name string
		due  []RepoIntegration
	}{
		{"the refusal listed first", []RepoIntegration{refused, unreachable}},
		{"the unreachable bot listed first", []RepoIntegration{unreachable, refused}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o.Integrations = orderedIntegrations{RepoIntegrationStore: integrations, due: tc.due}
			rotated, err := o.RotateDueProjectBots(ctx, 0)
			if rotated != 0 {
				t.Fatalf("rotated = %d, want 0: both rotations failed", rotated)
			}
			if err == nil {
				t.Fatal("RotateDueProjectBots reported no failure for two failing rotations")
			}
			if !errors.Is(err, sweepRefusal) {
				t.Errorf("the refusal did not reach the caller: %v", err)
			}
			var dns *net.DNSError
			if !errors.As(err, &dns) {
				t.Errorf("the unreachable rotation did not reach the caller: %v", err)
			}
			if !lease.TransportFailure(err) {
				t.Errorf("lease.TransportFailure = false for a sweep that met an unreachable bot: %v", err)
			}
			aggregates[tc.name] = err.Error()
		})
	}
	o.Integrations = integrations
	var texts []string
	for _, text := range aggregates {
		texts = append(texts, text)
	}
	if len(texts) != 2 {
		t.Fatalf("only %d of the 2 orders produced an aggregate, so the claim that both read the same was never tested: %q", len(texts), texts)
	}
	if texts[0] != texts[1] {
		t.Errorf("the aggregate depends on the order the query listed the integrations in:\n%q\nvs\n%q", texts[0], texts[1])
	}
}

// The collector is built for the sweep its own key discipline describes: one
// whose items run in PARALLEL, where arrival order is not a fact. So it
// records from several goroutines without losing a cause and without a data
// race — the contract lease.Pass states for the same job, one site up.
func TestSweepCauses_CollectsFromParallelItems(t *testing.T) {
	const items = 32
	var causes sweepCauses
	// A gate rather than a budget: every goroutine is waiting before any of
	// them records, so the interleaving is real on any machine.
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(items)
	done.Add(items)
	for i := range items {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			// Half the items read the aggregate while the others add to it:
			// the reader is the second goroutine the mutex answers for.
			// The key is zero-padded, and the message carries it, so the
			// aggregate reading in key order and reading in message order
			// are the same claim — asserted below without a second table.
			key := fmt.Sprintf("item-%02d", i)
			if i%2 == 0 {
				causes.add(key, fmt.Errorf("%s refused", key))
				return
			}
			_ = causes.err()
			causes.add(key, fmt.Errorf("%s refused", key))
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	err := causes.err()
	if err == nil {
		t.Fatal("the collector lost every cause of a parallel sweep")
	}
	var multi interface{ Unwrap() []error }
	if !errors.As(err, &multi) {
		t.Fatalf("the aggregate of %d items is not a multi-cause error: %v", items, err)
	}
	if got := len(multi.Unwrap()); got != items {
		t.Fatalf("the aggregate carries %d causes, want the %d the sweep recorded", got, items)
	}
	// Ordered by the ITEM's key, not by the goroutine that won: the same set
	// of failures reads identically whatever order they arrived in.
	var keyed []string
	for _, cause := range multi.Unwrap() {
		keyed = append(keyed, cause.Error())
	}
	if !slices.IsSorted(keyed) {
		t.Errorf("the aggregate reads in arrival order, not key order: %q", keyed)
	}
}
