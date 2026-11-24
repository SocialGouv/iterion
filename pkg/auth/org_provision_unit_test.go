package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// CreateOrgFor used to read AFTER its first irreversible write: it wrote the
// org and its owner row, then called CreateTeamFor, whose three reads
// (account, org, org membership) could fail with the org already standing —
// a TEAM-LESS org holding the slug, repairable by a super-admin only, whose
// retry answered acme-corp-2. Two properties, two scenarios: every read runs
// before the first write, and a write that fails leaves the state the call
// started from.

// orgProvisionStore records, in order, every call the provisioning makes —
// reads, writes, and the writes of a compensation — and can fail the k-th
// WRITE. The recorder is fed by the call under test and read by the test, so
// it takes a mutex.
type orgProvisionStore struct {
	identity.Store

	mu        sync.Mutex
	calls     []string
	writes    int
	failWrite int // 1-based index of the write that fails; 0 = none
	err       error
}

func newOrgProvisionStore(st identity.Store) *orgProvisionStore {
	return &orgProvisionStore{Store: st, err: errStoreUnreachable}
}

// failNthWrite arms the gate on the k-th write (counted from now), reusing
// the fail-the-Nth-call shape of the accept paths' harness.
func (s *orgProvisionStore) failNthWrite(k int) *orgProvisionStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = 0
	s.failWrite = k
	return s
}

func (s *orgProvisionStore) disarm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWrite = 0
}

func (s *orgProvisionStore) record(entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, entry)
}

// write records a write and answers the armed failure for the k-th one.
func (s *orgProvisionStore) write(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	s.calls = append(s.calls, "write "+name)
	if s.failWrite != 0 && s.writes == s.failWrite {
		return fmt.Errorf("identity: %s: %w", name, s.err)
	}
	return nil
}

func (s *orgProvisionStore) sequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *orgProvisionStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
	s.writes = 0
}

func (s *orgProvisionStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	s.record("read GetUser")
	return s.Store.GetUser(ctx, id)
}

func (s *orgProvisionStore) GetOrg(ctx context.Context, id string) (identity.Org, error) {
	s.record("read GetOrg")
	return s.Store.GetOrg(ctx, id)
}

func (s *orgProvisionStore) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	s.record("read GetOrgMembership")
	return s.Store.GetOrgMembership(ctx, userID, orgID)
}

func (s *orgProvisionStore) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	s.record("read GetTeam")
	return s.Store.GetTeam(ctx, id)
}

func (s *orgProvisionStore) CreateOrg(ctx context.Context, o identity.Org) (identity.Org, error) {
	if err := s.write("CreateOrg"); err != nil {
		return identity.Org{}, err
	}
	return s.Store.CreateOrg(ctx, o)
}

func (s *orgProvisionStore) UpsertOrgMembership(ctx context.Context, m identity.OrgMembership) error {
	if err := s.write("UpsertOrgMembership"); err != nil {
		return err
	}
	return s.Store.UpsertOrgMembership(ctx, m)
}

func (s *orgProvisionStore) CreateTeam(ctx context.Context, t identity.Team) (identity.Team, error) {
	if err := s.write("CreateTeam"); err != nil {
		return identity.Team{}, err
	}
	return s.Store.CreateTeam(ctx, t)
}

func (s *orgProvisionStore) UpsertMembership(ctx context.Context, m identity.Membership) error {
	if err := s.write("UpsertMembership"); err != nil {
		return err
	}
	return s.Store.UpsertMembership(ctx, m)
}

// The undo writes are recorded apart: they are the compensation, not writes a
// failure may be armed on.
func (s *orgProvisionStore) DeleteOrg(ctx context.Context, id string) error {
	s.record("undo DeleteOrg")
	return s.Store.DeleteOrg(ctx, id)
}

func (s *orgProvisionStore) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	s.record("undo DeleteOrgMembership")
	return s.Store.DeleteOrgMembership(ctx, userID, orgID)
}

func (s *orgProvisionStore) DeleteTeam(ctx context.Context, id string) error {
	s.record("undo DeleteTeam")
	return s.Store.DeleteTeam(ctx, id)
}

func (s *orgProvisionStore) DeleteMembership(ctx context.Context, userID, teamID string) error {
	s.record("undo DeleteMembership")
	return s.Store.DeleteMembership(ctx, userID, teamID)
}

// seedOrgProvisioner answers a service on the recording store and the id of
// the super-admin the org is provisioned for.
func seedOrgProvisioner(t *testing.T) (*Service, *orgProvisionStore, string) {
	t.Helper()
	svc := newTestService(t, SignupOpen)
	admin, _, err := svc.CreateUserAndPersonalTeam(context.Background(), "admin@example.com", "Admin", "correcthorse", true, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("bootstrap the super-admin: %v", err)
	}
	gate := newOrgProvisionStore(svc.store)
	svc.SetStoreForTest(gate)
	gate.reset()
	return svc, gate, admin.ID
}

func TestCreateOrgFor_everyReadRunsBeforeTheFirstWrite(t *testing.T) {
	svc, gate, adminID := seedOrgProvisioner(t)

	org, err := svc.CreateOrgFor(context.Background(), adminID, "Acme Corp", "")
	if err != nil {
		t.Fatalf("CreateOrgFor: %v", err)
	}
	if org.Slug != "acme-corp" {
		t.Fatalf("slug = %q, want acme-corp", org.Slug)
	}
	seq := gate.sequence()
	firstWrite, lastRead := -1, -1
	for i, c := range seq {
		switch {
		case strings.HasPrefix(c, "write ") && firstWrite < 0:
			firstWrite = i
		case strings.HasPrefix(c, "read "):
			lastRead = i
		}
	}
	if firstWrite < 0 {
		t.Fatalf("the provisioning wrote nothing: %v", seq)
	}
	if lastRead > firstWrite {
		t.Errorf("a read runs after the first irreversible write (%s at %d, first write at %d): %v",
			seq[lastRead], lastRead, firstWrite, seq)
	}
	// A provisioning that succeeded compensates nothing.
	for _, c := range seq {
		if strings.HasPrefix(c, "undo ") {
			t.Errorf("a successful provisioning ran a compensation: %v", seq)
			break
		}
	}
}

func TestCreateOrgFor_aFailedWriteLeavesNoOrgAndTheRetryKeepsTheSlug(t *testing.T) {
	ctx := context.Background()
	// The writes of the provisioning, in order: the org row, the owner org
	// membership, the default team, its owner grant. Failing the first
	// leaves nothing by construction; the other three are the ones that
	// used to leave a team-less org holding the slug.
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("the %s write fails", []string{"first (org)", "second (owner membership)", "third (default team)", "fourth (team grant)"}[k-1]), func(t *testing.T) {
			svc, gate, adminID := seedOrgProvisioner(t)
			gate.failNthWrite(k)

			_, err := svc.CreateOrgFor(ctx, adminID, "Acme Corp", "")
			if err == nil {
				t.Fatalf("write %d failed and the provisioning answered success", k)
			}
			if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("err = %v, want the store failure of write %d", err, k)
			}
			// Nothing of the provisioning stands: the slug is free.
			if o, gerr := svc.store.GetOrgBySlug(ctx, "acme-corp"); !errors.Is(gerr, identity.ErrNotFound) {
				t.Fatalf("write %d left the org %+v standing (read err %v): its slug is taken and the retry answers acme-corp-2", k, o, gerr)
			}

			// And the retry — the only remedy, super-admin only — gets the
			// name that was asked for.
			gate.disarm()
			org, err := svc.CreateOrgFor(ctx, adminID, "Acme Corp", "")
			if err != nil {
				t.Fatalf("the retry after write %d failed: %v", k, err)
			}
			if org.Slug != "acme-corp" {
				t.Fatalf("the retry after write %d answered slug %q, want acme-corp", k, org.Slug)
			}
			// The org the retry answers is usable: it has its default team,
			// and that team carries the NAME asked for — an orphan team row
			// left standing by the first attempt holds the slug and pushes
			// the retry's own team to acme-corp-2, the same defect one level
			// down.
			teams, err := svc.store.ListTeamsByOrg(ctx, org.ID)
			if err != nil || len(teams) != 1 {
				t.Fatalf("the org the retry after write %d created has %d teams (err %v), want its default team", k, len(teams), err)
			}
			if teams[0].Slug != "acme-corp" {
				t.Fatalf("the default team of the retry after write %d has slug %q, want acme-corp: the first attempt left a team row holding it", k, teams[0].Slug)
			}
		})
	}
}

// TestCreateTeamFor_aFailedGrantLeavesNoTeam is the standalone team path's own
// isolate: the same compensation, reached through the public CreateTeamFor —
// two guards, two scenarios, or neither is proven.
func TestCreateTeamFor_aFailedGrantLeavesNoTeam(t *testing.T) {
	ctx := context.Background()
	svc, gate, adminID := seedOrgProvisioner(t)
	org, err := svc.CreateOrgFor(ctx, adminID, "Acme Corp", "")
	if err != nil {
		t.Fatalf("seed the org: %v", err)
	}
	// The team's own two writes: the row, then the owner grant.
	gate.failNthWrite(2)

	if _, err := svc.CreateTeamFor(ctx, adminID, org.ID, "QE", ""); err == nil {
		t.Fatal("the grant failed and CreateTeamFor answered success")
	}
	if tm, gerr := svc.store.GetTeamBySlug(ctx, "qe"); !errors.Is(gerr, identity.ErrNotFound) {
		t.Fatalf("the team %+v stands with no owner grant behind it (read err %v): the retry answers qe-2", tm, gerr)
	}
	gate.disarm()
	tm, err := svc.CreateTeamFor(ctx, adminID, org.ID, "QE", "")
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if tm.Slug != "qe" {
		t.Fatalf("the retry answered slug %q, want qe", tm.Slug)
	}
}

// TestProvisionUndo_namesTheRowItCouldNotRemove: a compensation that itself
// fails does not hide it — the caller keeps the original cause (what it
// asked about) AND learns which row a retry will collide with.
func TestProvisionUndo_namesTheRowItCouldNotRemove(t *testing.T) {
	cause := errors.New("the write that stopped the provisioning")
	undo := &provisionUndo{}
	undo.push("the org o1 (slug acme-corp)", func(context.Context) error { return errStoreUnreachable })
	undo.push("the row already gone", func(context.Context) error { return identity.ErrNotFound })

	err := undo.compensate(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("the compensation lost the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "the org o1 (slug acme-corp)") {
		t.Errorf("the compensation did not name the row it could not remove: %v", err)
	}
	if strings.Contains(err.Error(), "the row already gone") {
		t.Errorf("a row already gone is the state the undo wanted, not a leftover: %v", err)
	}
	// A compensation with nothing committed answers the cause untouched.
	if got := (&provisionUndo{}).compensate(context.Background(), cause); got != cause {
		t.Errorf("an empty compensation changed the cause: %v", got)
	}
}

// TestProvisionUndo_runsOnADoneContext: the provisioning often fails BECAUSE
// the caller went away, so the undo cannot inherit its cancellation — and it
// must stay bounded.
func TestProvisionUndo_runsOnADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	undo := &provisionUndo{}
	undo.push("the org o1", func(c context.Context) error {
		ran = true
		if err := c.Err(); err != nil {
			return fmt.Errorf("the compensation inherited the caller's cancellation: %w", err)
		}
		if _, ok := c.Deadline(); !ok {
			return errors.New("the compensation context is unbounded")
		}
		return nil
	})
	if err := undo.compensate(ctx, errors.New("cause")); strings.Contains(err.Error(), "could not remove") {
		t.Fatalf("the compensation could not run on a done context: %v", err)
	}
	if !ran {
		t.Fatal("the compensation did not run")
	}
}
