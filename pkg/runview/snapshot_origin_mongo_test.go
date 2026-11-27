package runview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestSnapshotOriginNativeMongo runs the snapshot origin matrix against the
// native Mongo store; CI's mongo-conformance job supplies the URI.
func TestSnapshotOriginNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	for _, f := range []store.ValueFormat{1, 2} {
		for _, mode := range snapshotOriginModes {
			t.Run(fmt.Sprintf("f%d/%s", f, mode), func(t *testing.T) {
				ctx := store.WithIdentity(t.Context(), "snapshot-tenant", "owner")
				snapshotOriginMatrix(t, snapshotMongoStore(t, uri, ctx), ctx, f, mode)
			})
		}
		// The tenant row: team-blind, no tenant filter hides B and the bound
		// scan compares the tenant; tenant-scoped, B is outside the query and
		// the scan answers the absence of its birth (ErrRunDataIdentityChanged).
		for _, scope := range []string{"blind", "scoped"} {
			t.Run(fmt.Sprintf("f%d/tenant-%s", f, scope), func(t *testing.T) {
				snapshotOriginTenantMongo(t, uri, f, scope)
			})
		}
		// The tenant-less row: a run created team-blind records no tenant
		// and its observation binds "" — a value the bound scan compares.
		t.Run(fmt.Sprintf("f%d/tenant-none", f), func(t *testing.T) {
			snapshotOriginNoTenantMongo(t, uri, f)
		})
	}
	for _, forge := range []string{"duplicate", "alias"} {
		t.Run("f1/lenient-tenant-"+forge, func(t *testing.T) {
			snapshotOriginLenientTenantMongo(t, uri, forge)
		})
	}
}

func snapshotMongoStore(t *testing.T, uri string, ctx context.Context) *mongostore.Store {
	t.Helper()
	s, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("snapshot_%d", time.Now().UnixNano()), Blob: snapshotExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.DB().Drop(c)
		_ = s.Close(c)
	})
	return s
}

// mongoSnapshotRun creates run r in format f under the requester's tenant
// "snapshot-tenant" (creation stamps it) with one event marked "A", and
// returns the store, the tenant-scoped ctx and the run's canonical
// observation, read team-blind by the stream selection's parsers.
func mongoSnapshotRun(t *testing.T, uri string, f store.ValueFormat) (*mongostore.Store, context.Context, store.RunObservation) {
	t.Helper()
	ctx := store.WithIdentity(t.Context(), "snapshot-tenant", "owner")
	s := snapshotMongoStore(t, uri, ctx)
	a, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.BindRunValueFormat(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendEvent(write, "r", store.Event{Data: map[string]any{"marker": "A"}}); err != nil {
		t.Fatal(err)
	}
	o, err := mongoCanonicalObservation(t, s)
	if err != nil || o.TenantID() != "snapshot-tenant" || o.Header().BirthID != a.BirthID || o.Header().BirthID == "" || o.Header().Format != f {
		t.Fatalf("fixture: canonical observation %+v tenant %q, %v; created birth %q format %d", o.Header(), o.TenantID(), err, a.BirthID, f)
	}
	return s, ctx, o
}

func mongoCanonicalObservation(t *testing.T, s *mongostore.Store) (store.RunObservation, error) {
	t.Helper()
	sel, err := s.ResolveStreamSelection(store.WithoutTenantFilter(t.Context()), "r")
	if err != nil {
		return store.RunObservation{}, err
	}
	return sel.EventStreamObservation(), nil
}

// rewriteMongoRunDocument replaces run r's document with edit(document): the
// original is deleted and the edited copy inserted in its place, every member
// edit does not touch kept with its BSON type.
func rewriteMongoRunDocument(t *testing.T, s *mongostore.Store, edit func(bson.D) bson.D) {
	t.Helper()
	blind := store.WithoutTenantFilter(t.Context())
	runs := s.DB().Collection("runs")
	raw, err := runs.FindOne(blind, bson.M{"_id": "r"}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.D
	if err = bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc = edit(doc)
	if _, err = runs.DeleteOne(blind, bson.M{"_id": "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err = runs.InsertOne(blind, doc); err != nil {
		t.Fatal(err)
	}
}

func withoutTenantID(doc bson.D) bson.D {
	kept := bson.D{}
	for _, e := range doc {
		if e.Key != "tenant_id" {
			kept = append(kept, e)
		}
	}
	return kept
}

// snapshotOriginTenantMongo: A records tenant "snapshot-tenant"; as the scan
// begins B replaces A with the same birth, format and protocol and tenant
// "other-tenant". BuildSnapshot binds the loaded run's tenant: the scan
// refuses exactly ErrRunDataIdentityChanged before any event is folded.
func snapshotOriginTenantMongo(t *testing.T, uri string, f store.ValueFormat, scope string) {
	s, scoped, o := mongoSnapshotRun(t, uri, f)
	ctx := scoped
	if scope == "blind" {
		ctx = store.WithoutTenantFilter(t.Context())
	}
	control := &snapshotOriginBarrier{RunStore: s}
	if snap, err := BuildSnapshot(ctx, control, "r"); err != nil || snap == nil || control.scanned != 1 || control.seen != "A" {
		t.Fatalf("control: snapshot=%v err=%v scanned=%d marker=%q", snap != nil, err, control.scanned, control.seen)
	}
	forge := func() {
		rewriteMongoRunDocument(t, s, func(doc bson.D) bson.D {
			return append(withoutTenantID(doc), bson.E{Key: "tenant_id", Value: "other-tenant"})
		})
		b, err := mongoCanonicalObservation(t, s)
		if err != nil || b.Header() != o.Header() || b.TenantID() != "other-tenant" {
			t.Fatalf("fixture: forged successor observed %+v tenant %q, %v; want A's header %+v and \"other-tenant\"", b.Header(), b.TenantID(), err, o.Header())
		}
	}
	wrapped := &snapshotOriginBarrier{RunStore: s, beforeScan: forge}
	snap, err := BuildSnapshot(ctx, wrapped, "r")
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || snap != nil || wrapped.scanned != 0 {
		t.Fatalf("%s: tenant replaced as the scan begins: snapshot=%v err=%v scanned=%d marker=%q, want ErrRunDataIdentityChanged and nothing scanned", scope, snap != nil, err, wrapped.scanned, wrapped.seen)
	}
}

// snapshotOriginNoTenantMongo: A is created team-blind and records no tenant,
// so the observation BuildSnapshot binds carries "" — a value, not
// "unobserved". As the scan begins B replaces A with the same birth, format
// and protocol and tenant "other-tenant": the team-blind BuildSnapshot refuses
// exactly ErrRunDataIdentityChanged before any event is folded.
func snapshotOriginNoTenantMongo(t *testing.T, uri string, f store.ValueFormat) {
	blind := store.WithoutTenantFilter(t.Context())
	s := snapshotMongoStore(t, uri, blind)
	a, err := s.CreateRunWithValueFormat(blind, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.BindRunValueFormat(blind, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendEvent(write, "r", store.Event{Data: map[string]any{"marker": "A"}}); err != nil {
		t.Fatal(err)
	}
	o, err := mongoCanonicalObservation(t, s)
	if err != nil || !o.Observed() || o.TenantID() != "" || o.Header().BirthID != a.BirthID || o.Header().BirthID == "" || o.Header().Format != f {
		t.Fatalf("fixture: canonical observation %+v tenant %q observed %t, %v; created birth %q format %d", o.Header(), o.TenantID(), o.Observed(), err, a.BirthID, f)
	}
	control := &snapshotOriginBarrier{RunStore: s}
	if snap, err := BuildSnapshot(blind, control, "r"); err != nil || snap == nil || control.scanned != 1 || control.seen != "A" {
		t.Fatalf("control: snapshot=%v err=%v scanned=%d marker=%q", snap != nil, err, control.scanned, control.seen)
	}
	forge := func() {
		rewriteMongoRunDocument(t, s, func(doc bson.D) bson.D {
			return append(withoutTenantID(doc), bson.E{Key: "tenant_id", Value: "other-tenant"})
		})
		b, err := mongoCanonicalObservation(t, s)
		if err != nil || b.Header() != o.Header() || b.TenantID() != "other-tenant" {
			t.Fatalf("fixture: forged successor observed %+v tenant %q, %v; want A's header %+v and \"other-tenant\"", b.Header(), b.TenantID(), err, o.Header())
		}
	}
	wrapped := &snapshotOriginBarrier{RunStore: s, beforeScan: forge}
	snap, err := BuildSnapshot(blind, wrapped, "r")
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || snap != nil || wrapped.scanned != 0 {
		t.Fatalf("observed \"\", B records \"other-tenant\" as the scan begins: snapshot=%v err=%v scanned=%d marker=%q, want ErrRunDataIdentityChanged and nothing scanned", snap != nil, err, wrapped.scanned, wrapped.seen)
	}
}

// snapshotOriginLenientTenantMongo: the run document carries a duplicated
// or case-alias tenant_id, which the struct decoder alone would read
// leniently and the canonical parser refuses. Team-blind, LoadRun,
// BuildSnapshot and CaptureRunOriginIn all refuse it typed, so no lenient
// capture exists for a bound read to inherit.
func snapshotOriginLenientTenantMongo(t *testing.T, uri, forge string) {
	s, _, o := mongoSnapshotRun(t, uri, store.LegacyValueFormat)
	rewriteMongoRunDocument(t, s, func(doc bson.D) bson.D {
		doc = withoutTenantID(doc)
		if forge == "duplicate" {
			return append(append(bson.D{{Key: "tenant_id", Value: "other-tenant"}}, doc...), bson.E{Key: "tenant_id", Value: "snapshot-tenant"})
		}
		return append(bson.D{{Key: "TENANT_ID", Value: "snapshot-tenant"}}, doc...)
	})
	blind := store.WithoutTenantFilter(t.Context())
	if h, err := store.ObserveRunDataHeader(blind, s, "r"); err != nil || h != o.Header() {
		t.Fatalf("fixture: forged header %+v, %v, want %+v", h, err, o.Header())
	}
	raw, err := s.DB().Collection("runs").FindOne(blind, bson.M{"_id": "r"}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	elements, err := raw.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range elements {
		if strings.EqualFold(e.Key(), "tenant_id") {
			keys = append(keys, e.Key())
		}
	}
	if want := map[string]string{"duplicate": "[tenant_id tenant_id]", "alias": "[TENANT_ID]"}[forge]; fmt.Sprint(keys) != want {
		t.Fatalf("fixture: stored tenant keys %q, want %s", keys, want)
	}
	if r, err := s.LoadRun(blind, "r"); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
		t.Fatalf("team-blind LoadRun of a %s tenant_id = %v, %v, want ErrInvalidRunDataIdentity", forge, r, err)
	}

	wrapped := &snapshotOriginBarrier{RunStore: s}
	snap, err := BuildSnapshot(blind, wrapped, "r")
	if !errors.Is(err, store.ErrInvalidRunDataIdentity) || snap != nil || wrapped.scanned != 0 {
		t.Fatalf("BuildSnapshot of a %s tenant_id: snapshot=%v err=%v scanned=%d, want ErrInvalidRunDataIdentity and nothing scanned", forge, snap != nil, err, wrapped.scanned)
	}
	if o, err := CaptureRunOriginIn(blind, s, "r"); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
		t.Fatalf("CaptureRunOriginIn of a %s tenant_id = tenant %q, %v, want ErrInvalidRunDataIdentity: no lenient capture to bind", forge, o.TenantID(), err)
	}
}

type snapshotBlob struct{}

func (snapshotBlob) PutArtifact(context.Context, string, string, int, []byte) error { return nil }
func (snapshotBlob) GetArtifact(context.Context, string, string, int) ([]byte, error) {
	return nil, fmt.Errorf("snapshotBlob: not found")
}
func (snapshotBlob) ListArtifactVersions(context.Context, string, string) ([]int, error) {
	return nil, nil
}
func (snapshotBlob) DeleteRun(context.Context, string) error { return nil }
func (snapshotBlob) Ping(context.Context) error              { return nil }
func (snapshotBlob) Close() error                            { return nil }
func (snapshotBlob) PutAttachment(context.Context, string, string, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetAttachment(context.Context, string, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, fmt.Errorf("snapshotBlob: not found")
}
func (snapshotBlob) PresignAttachment(context.Context, string, string, string, time.Duration) (string, error) {
	return "", fmt.Errorf("snapshotBlob: unsupported")
}
func (snapshotBlob) DeleteAttachment(context.Context, string, string, string) error { return nil }
func (snapshotBlob) DeleteRunAttachments(context.Context, string) error             { return nil }
func (snapshotBlob) PutToolBlob(context.Context, string, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetToolBlobRange(context.Context, string, string, string, int64, int64) ([]byte, int64, bool, error) {
	return nil, 0, false, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunToolBlobs(context.Context, string) error { return nil }
func (snapshotBlob) PutRunFile(context.Context, string, string, string, io.Reader, int64) error {
	return nil
}
func (snapshotBlob) ListRunFiles(context.Context, string) ([]blob.RunFileObject, error) {
	return nil, nil
}
func (snapshotBlob) GetRunFile(context.Context, string, string) (io.ReadCloser, blob.RunFileObject, error) {
	return nil, blob.RunFileObject{}, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunFiles(context.Context, string) error { return nil }
func (snapshotBlob) PutIRBlob(context.Context, string, []byte) error {
	return nil
}
func (snapshotBlob) GetIRBlob(context.Context, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunIR(context.Context, string) error { return nil }
func (snapshotBlob) PutBackendSession(context.Context, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetBackendSession(context.Context, string, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteBackendSession(context.Context, string, string) error { return nil }
func (snapshotBlob) PutUpload(context.Context, string, string, string, io.ReadSeeker, int64) error {
	return nil
}
func (snapshotBlob) GetUpload(context.Context, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteUpload(context.Context, string) error             { return nil }
func (snapshotBlob) DeleteRunBackendSessions(context.Context, string) error { return nil }

var _ blob.Client = snapshotBlob{}

type snapshotExactBlob struct{ snapshotBlob }

func (b snapshotExactBlob) PutArtifactForFormat(c context.Context, _ int, r, n string, v int, d []byte) error {
	return b.PutArtifact(c, r, n, v, d)
}
func (b snapshotExactBlob) GetArtifactForFormat(c context.Context, _ int, r, n string, v int) ([]byte, error) {
	return b.GetArtifact(c, r, n, v)
}
func (b snapshotExactBlob) ListArtifactVersionsForFormat(c context.Context, _ int, r, n string) ([]int, error) {
	return b.ListArtifactVersions(c, r, n)
}
func (b snapshotExactBlob) DeleteRunArtifactsForFormat(c context.Context, _ int, r string) error {
	return b.DeleteRun(c, r)
}
func (b snapshotExactBlob) DeleteRunArtifacts(c context.Context, r string) error {
	return b.DeleteRun(c, r)
}
func (b snapshotExactBlob) RunObjectsForFormat(int) (blob.RunObjects, error) { return b, nil }
func (b snapshotExactBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return b, nil
}
func (b snapshotExactBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return b, nil
}
