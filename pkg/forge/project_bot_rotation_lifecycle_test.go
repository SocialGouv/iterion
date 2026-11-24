package forge

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"testing"
	"time"
)

var errRotationMatrixWriteFailure = errors.New("rotationMatrix injected secret write failure")
var errRotationMatrixSealFailure = errors.New("rotationMatrix injected seal failure")
var errRotationMatrixRevokeFailure = errors.New("rotationMatrix injected forge unavailable")
var errRotationMatrixRecordFailure = errors.New("rotationMatrix injected durable store unavailable")

type rotationMatrixSecrets struct {
	secrets.GenericSecretStore
	stage      string
	once       bool
	transition func()
}

func (s *rotationMatrixSecrets) Update(ctx context.Context, g secrets.GenericSecret) error {
	if s.once {
		return s.GenericSecretStore.Update(ctx, g)
	}
	s.once = true
	if s.stage == "saved" {
		if e := s.GenericSecretStore.Update(ctx, g); e != nil {
			return e
		}
		s.transition()
		return nil
	}
	s.transition()
	return errRotationMatrixWriteFailure
}

type rotationMatrixSealer struct {
	secrets.Sealer
	once       bool
	transition func()
}

func (s *rotationMatrixSealer) Seal(p, a []byte) ([]byte, error) {
	if s.once {
		return s.Sealer.Seal(p, a)
	}
	s.once = true
	s.transition()
	return nil, errRotationMatrixSealFailure
}

type rotationMatrixRecord struct {
	RepoIntegrationStore
	adopt bool
	fail  bool
	once  bool
}

func (s *rotationMatrixRecord) RecordProjectBot(ctx context.Context, id, sec, token string, pb *ProjectBot, at time.Time) error {
	if !s.once {
		s.once = true
		if s.fail {
			return errRotationMatrixRecordFailure
		}
		if s.adopt {
			if e := s.RepoIntegrationStore.RecordProjectBot(ctx, id, sec, token, pb, at); e != nil {
				return e
			}
			return ErrIntegrationConflict
		}
	}
	return s.RepoIntegrationStore.RecordProjectBot(ctx, id, sec, token, pb, at)
}

type rotationMatrixClient struct {
	Admin
	rot          SelfRotator
	t            *testing.T
	compensating *bool
	deny         bool
}

func (c *rotationMatrixClient) RotateSelf(ctx context.Context, repo string, exp time.Time) (ProjectBotCredential, error) {
	return c.rot.RotateSelf(ctx, repo, exp)
}
func (c *rotationMatrixClient) RevokeSelf(ctx context.Context) error {
	if *c.compensating {
		if ctx.Err() != nil {
			c.t.Fatalf("compensation inherited canceled request: %v", ctx.Err())
		}
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) <= 0 || time.Until(dl) > time.Minute {
			c.t.Fatalf("compensation context unbounded: deadline=%v present=%v", dl, ok)
		}
		if c.deny {
			return errRotationMatrixRevokeFailure
		}
	}
	return c.rot.RevokeSelf(ctx)
}
func TestRotationLifecycleMatrix(t *testing.T) {
	for _, stage := range []string{"saved", "write-error", "seal-error"} {
		for _, life := range []string{"same", "retired", "reactivated", "disconnected", "adopted", "retired-canceled", "retired-revoke-fails", "same-record-fails"} {
			t.Run(stage+"/"+life, func(t *testing.T) {
				o, f, conn := newProjectBotOrch(t)
				base := context.Background()
				ctx, cancel := context.WithCancel(base)
				defer cancel()
				f.lifetime = tenDays
				ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
				compensating := false
				oldFactory := o.AdminForToken
				o.AdminForToken = func(p Provider, url, token string) (Admin, error) {
					if p != conn.Provider || url != conn.BaseURL() {
						t.Fatalf("wrong captured endpoint %s %s", p, url)
					}
					a, e := oldFactory(p, url, token)
					if e != nil {
						return nil, e
					}
					sr, ok := a.(SelfRotator)
					if !ok {
						t.Fatal("fixture lacks self rotator")
					}
					return &rotationMatrixClient{Admin: a, rot: sr, t: t, compensating: &compensating, deny: life == "retired-revoke-fails" || life == "same-record-fails"}, nil
				}
				if life == "adopted" || life == "same-record-fails" {
					o.Integrations = &rotationMatrixRecord{RepoIntegrationStore: o.Integrations, adopt: life == "adopted", fail: life == "same-record-fails"}
				}
				transition := func() {
					switch life {
					case "retired", "retired-canceled", "retired-revoke-fails", "reactivated":
						if e := o.Deprovision(base, ri.TenantID, ri.ID); e != nil {
							t.Fatal(e)
						}
						if life == "reactivated" {
							if _, e := o.Provision(base, ProvisionRequest{TenantID: ri.TenantID, ConnectionID: conn.ID, RepoFullName: ri.RepoFullName, BotIDs: []string{"review-pr"}, ActorID: "u1"}); e != nil {
								t.Fatal(e)
							}
						}
					case "disconnected":
						if e := o.DeprovisionConnection(base, ri.TenantID, conn.ID); e != nil {
							t.Fatal(e)
						}
					default:
						cur, e := o.Integrations.Get(base, ri.ID)
						if e != nil {
							t.Fatal(e)
						}
						cur.SyncIssuesEnabled = true
						if e = o.Integrations.Update(base, cur); e != nil {
							t.Fatal(e)
						}
					}
					compensating = true
					if life == "retired-canceled" {
						cancel()
					}
				}
				if stage == "seal-error" {
					o.Sealer = &rotationMatrixSealer{Sealer: o.Sealer, transition: transition}
				} else {
					o.Secrets = &rotationMatrixSecrets{GenericSecretStore: o.Secrets, stage: stage, transition: transition}
				}
				n, e := o.RotateDueProjectBots(ctx, 0)
				t.Logf("first n=%d error=%v live=%d", n, e, len(f.live()))
				if life == "same-record-fails" {
					if e == nil || n != 0 {
						t.Fatalf("unavailable stores reported success n=%d err=%v", n, e)
					}
					return
				}
				if life == "retired-revoke-fails" {
					if stage != "saved" && !errors.Is(e, errRotationMatrixRevokeFailure) {
						t.Fatalf("remote compensation failure lost identity: %v", e)
					}
					if stage != "saved" && len(f.live()) != 1 {
						t.Fatal("fixture failed to keep live issued token")
					}
					return
				}
				if life == "same" || life == "adopted" {
					cur, ge := o.Integrations.Get(base, ri.ID)
					if ge != nil {
						t.Fatal(ge)
					}
					if !cur.SyncIssuesEnabled || cur.ProjectBot.TokenID == ri.ProjectBot.TokenID {
						t.Fatalf("credential or operator setting lost: %+v", cur)
					}
					if len(f.live()) != 1 || f.live()[0].id != cur.ProjectBot.TokenID {
						t.Fatal("adopted credential revoked or orphaned")
					}
					if stage != "saved" {
						compensating = false
						n, e = o.RotateDueProjectBots(base, 0)
						if e != nil || n != 1 {
							t.Fatalf("failed write recovery n=%d error=%v", n, e)
						}
					}
					return
				}
				want := 0
				if life == "reactivated" {
					want = 1
				}
				if len(f.live()) != want {
					t.Fatalf("orphan: wanted%d live, got%d", want, len(f.live()))
				}
				if want == 1 {
					cur, ge := o.Integrations.Get(base, ri.ID)
					if ge != nil {
						t.Fatal(ge)
					}
					if f.live()[0].id != cur.ProjectBot.TokenID {
						t.Fatal("replacement credential revoked")
					}
				}
			})
		}
	}
}
