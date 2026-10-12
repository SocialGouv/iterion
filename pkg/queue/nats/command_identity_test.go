package nats

// Plan v2 section P6: the control-plane commands (cancel, steer) carry
// the run's admitted identity in their headers — the server stamps
// them from the run document's frozen stamp, and the pod holding the
// run verifies them against the message it admitted before acting. A
// command of another identity — other tenant OR other pool — or with
// no stamp at all is ignored: neither cancels nor steers a run across
// the boundary. Both halves of the boundary are asserted: the
// pool comparison exists for the cross-pool case and erodes silently
// if no witness ever sends a same-tenant foreign-pool command.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The commands ride core NATS subjects (no stream, no KV): a plain
// connection is enough, and the unique per-test run ids keep the
// subjects collision-free against the other packages' integration
// tests sharing the job's broker.
func commandConn(t *testing.T) *Conn {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping command identity tests (CI: nats-conformance job)")
	}
	nc, err := natsgo.Connect(uri,
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return &Conn{nc: nc, logger: iterlog.Nop()}
}

// TestControlPlaneCommandsCarryAdmittedIdentity drives the cancel
// command end to end: the honest stamp fires the handler; a foreign
// tenant, a same-tenant foreign pool, and a missing stamp do not.
func TestControlPlaneCommandsCarryAdmittedIdentity(t *testing.T) {
	conn := commandConn(t)
	ctx := context.Background()
	runID := fmt.Sprintf("run-cancel-id-%d", time.Now().UnixNano())

	honest := store.LeaseIdentity{TenantID: "team-a", Pool: "pool-a"}
	fired := make(chan struct{}, 4)
	sub, err := conn.SubscribeCancel(ctx, runID, honest, func() {
		fired <- struct{}{}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// The honest command fires the handler.
	if err := conn.CancelRun(runID, honest); err != nil {
		t.Fatalf("honest cancel: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("honest cancel did not fire the handler")
	}

	// Foreign identities never do — same subject, other boundary: the
	// other tenant, and the other POOL under the same tenant (the pool
	// is half the check; only ever sending tenant-foreign commands
	// would let a tenant-only comparison pass everything).
	for _, foreign := range []store.LeaseIdentity{
		{TenantID: "team-b", Pool: "pool-a"},
		{TenantID: "team-a", Pool: "pool-b"},
		{},
	} {
		if err := conn.CancelRun(runID, foreign); err != nil {
			t.Fatalf("foreign cancel publish %+v: %v", foreign, err)
		}
	}
	select {
	case <-fired:
		t.Fatal("a foreign or unstamped cancel fired the handler")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSteerCommandCarriesAdmittedIdentity drives the steer command
// through the REAL emitter: SteerRun publishes the stamp (asserted on
// the raw wire), the honest round-trip applies and replies, and
// foreign-tenant and same-tenant foreign-pool commands are never
// delivered to the handler.
func TestSteerCommandCarriesAdmittedIdentity(t *testing.T) {
	conn := commandConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runID := fmt.Sprintf("run-steer-id-%d", time.Now().UnixNano())

	honest := store.LeaseIdentity{TenantID: "team-a", Pool: "pool-a"}
	handled := make(chan struct{}, 4)
	sub, err := conn.SubscribeSteer(ctx, runID, honest, func(_ []byte, commandID string) {
		_ = conn.PublishSteerAck(runID, commandID, []byte("ok"))
		handled <- struct{}{}
	})
	if err != nil {
		t.Fatalf("subscribe steer: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// A raw subscriber watches the headers the real emitter publishes.
	raw, err := conn.nc.SubscribeSync(fmt.Sprintf(SubjectSteerFmt, runID))
	if err != nil {
		t.Fatalf("raw subscribe: %v", err)
	}
	defer func() { _ = raw.Unsubscribe() }()

	reply, err := conn.SteerRun(ctx, runID, []byte(`{"type":"bump_loop"}`), "cmd-1", honest)
	if err != nil {
		t.Fatalf("honest steer round-trip: %v", err)
	}
	if string(reply) != "ok" {
		t.Fatalf("honest steer reply: %q", reply)
	}
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("honest steer was not delivered to the handler")
	}

	wire, err := raw.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no steer command on the wire: %v", err)
	}
	if got := wire.Header.Get(HeaderAdmittedTenant); got != "team-a" {
		t.Fatalf("SteerRun published tenant %q, want team-a", got)
	}
	if got := wire.Header.Get(HeaderAdmittedPool); got != "pool-a" {
		t.Fatalf("SteerRun published pool %q, want pool-a", got)
	}

	// Foreign identities are never delivered: other tenant, and the
	// same tenant under another pool.
	for _, foreign := range []store.LeaseIdentity{
		{TenantID: "team-b", Pool: "pool-a"},
		{TenantID: "team-a", Pool: "pool-b"},
	} {
		msg := natsgo.NewMsg(fmt.Sprintf(SubjectSteerFmt, runID))
		msg.Header = natsgo.Header{}
		stampAdmitted(msg.Header, foreign)
		msg.Data = []byte(`{"type":"bump_loop"}`)
		if err := conn.nc.PublishMsg(msg); err != nil {
			t.Fatalf("publish steer %+v: %v", foreign, err)
		}
	}
	select {
	case <-handled:
		t.Fatal("a foreign steer was delivered to the handler")
	case <-time.After(300 * time.Millisecond):
	}
}
