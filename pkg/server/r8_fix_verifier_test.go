package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// r8RealCatalogManifest bakes one auto-provisionable bot into the platform
// catalog, so the approval arm can replay through the PRODUCTION lookups
// (s.forgeBotForge / s.forgeBotInvocations) instead of the test double the
// other witnesses wire — the double answers a missing bot with (nil, nil),
// which flows into the orchestrator's static refusal, while production
// answers it with a lookup error.
const r8RealCatalogManifest = `name: review-pr
display_name: Review PR
version: 1.0.0
invocations:
  - kind: schedule
    schedule:
      suggested_cron: 0 2 * * *
forge:
  events:
    - pull_request
`

const r8RealCatalogBot = "schema out:\n  ok: bool\n\ntool work:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow review_pr:\n  worktree: none\n  entry: work\n  work -> done\n"

// r8WireRealBotCatalog points the approval server's orchestrator at the real
// production lookups, with a catalog that has review-pr and nothing else.
func r8WireRealBotCatalog(t *testing.T, s *Server) {
	t.Helper()
	botsDir := t.TempDir()
	bundleDir := filepath.Join(botsDir, "review-pr")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte(r8RealCatalogBot), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(r8RealCatalogManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.botSources = botsource.NewMemoryStore()
	s.forgeOrchestrator.Bots = s.forgeBotForge
	s.forgeOrchestrator.Invocations = s.forgeBotInvocations
}

// A bot id outside the catalog parks (the park validates no ids) and its
// approve replay dies in the bot resolve — a refusal that is a property of
// the RECORDED bot set, so no retry of the record can land it. Production's
// lookup shape (an error, not (nil, nil)) must ride the same class sentinel
// the static refusals carry, or the arm keeps the record pending forever
// behind a 502 that reads as transient while the requester sees "awaiting
// approval". Witnessed end to end through the real lookups: with the
// catalog wired, the double is out of the loop entirely.
func TestR8_GhostBot_ProductionLookupShape_ApproveConsumesTheDeadEnd(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	r8WireRealBotCatalog(t, s)

	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 3 * * *"},"bot_ids":["review-pr","ghost-bot"]}`)

	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), "cannot land as recorded") {
		t.Fatalf("a bot outside the catalog is a deterministic dead end and must be consumed: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if !strings.Contains(aw.Body.String(), "ghost-bot") {
		t.Fatalf("the 409 must name the bot the re-submission must fix: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the dead-end record stayed pending: %v", got)
	}
	// The real lookups carry the class sentinel themselves — the fact the
	// arm's classifier reads, pinned at the production site, not the double.
	if _, err := s.forgeBotForge(context.Background(), "t1", "ghost-bot"); !errors.Is(err, forge.ErrBotNotProvisionable) {
		t.Fatalf("forgeBotForge must classify a missing bot as ErrBotNotProvisionable: %v", err)
	}
	if _, err := s.forgeBotInvocations(context.Background(), "t1", "ghost-bot"); !errors.Is(err, forge.ErrBotNotProvisionable) {
		t.Fatalf("forgeBotInvocations must classify a missing bot as ErrBotNotProvisionable: %v", err)
	}
	// Nothing landed: a consumed approval is not a provisioned one.
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(integ.BotIDs) != 1 {
		t.Fatalf("approve must not provision a refused set: %v (%v)", integ.BotIDs, err)
	}
}

// The class boundary's other side: a bot lookup that FAILS (the catalog store
// blipped) is not a fact about the recorded bot set, so it must NOT ride the
// deterministic class — the arm keeps the record pending behind the 502, and
// a retry after the store heals lands it. Consuming here would destroy a
// recoverable record.
func TestR8_LookupFailure_KeepsTheRecordPending(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	s.forgeOrchestrator.Bots = func(_ context.Context, _, _ string) (*bundle.ForgeRequirements, error) {
		return nil, errors.New("bot catalog store unavailable")
	}
	id := parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["review-pr","docs-refresh"]}`)

	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusBadGateway {
		t.Fatalf("a lookup failure is transient and must keep the record: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if strings.Contains(aw.Body.String(), "cannot land as recorded") {
		t.Fatalf("a lookup failure must not read as a deterministic refusal: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must stay pending for a retry that can land it: %v", got)
	}
}

// The arm-level witness for the CONSUMED half of the retry-class boundary on
// the ROUTING divergence: a record silent about routing replays into the
// pre-write two-store divergence refusal — deterministic, never landed by a
// retry of the same record — and approve consumes it with the divergence
// named. The launch-vars divergence has its own consumption witness
// (TestRV_R6VF_DivergedStores_ApproveConsumesTheRecord); without this twin,
// a retry-wrap added to the routing site flips deterministic dead ends back
// to pending-forever and nothing reds. The diverged state is modeled at the
// stores: it is the crashed two-store write the refusal exists for, and no
// shipped HTTP surface writes a routing block out-of-band on both records.
func TestR8_TwoStoreRoutingDivergence_ApproveConsumesTheRecord(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.Routing = &llmroute.Policy{PairOrder: []string{"claude/sub"}}
	if err := s.forgeIntegrations.Update(context.Background(), ri); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routing = &llmroute.Policy{PairOrder: []string{"openai/x"}}
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 3 * * *"}}`)
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), "diverge") {
		t.Fatalf("approve must consume a deterministically-diverged record with its reason: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the diverged record stayed pending after approve: %v", got)
	}
	// Nothing landed forge-side or store-side: a consumed approval is not a
	// provisioned one.
	integ, gerr := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if gerr != nil || len(integ.BotIDs) != 1 {
		t.Fatalf("approve must not provision a refused request: %v (%v)", integ.BotIDs, gerr)
	}
}

// The class table behind deterministicProvisionRefusal, pinned at the source:
// of the ErrProvisionDiverged wraps in the orchestrator, the SIX that carry
// ErrProvisionDivergedRetry are the retry-convergent flavors the approval arm
// SPARES (a re-run of the same request lands), and the TWO that do not are
// the pre-write two-store detections it CONSUMES (only a re-submission with
// the explicit gesture resolves them). A wrap moving between the classes is
// the one-line refactor that flips deterministic dead ends to
// pending-forever — or consumes a record a retry would have landed — so the
// move must edit this assertion, with the witnesses either way.
func TestR8_ProvisionDivergedWraps_ClassTable(t *testing.T) {
	const src = "../forge/orchestrator.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var retrySites, plainSites []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Errorf" {
			return true
		}
		hasDiverged, hasRetry := false, false
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				switch id.Name {
				case "ErrProvisionDiverged":
					hasDiverged = true
				case "ErrProvisionDivergedRetry":
					hasRetry = true
				}
			}
		}
		if !hasDiverged {
			return true
		}
		name := fmt.Sprintf("%s:%d", filepath.Base(src), fset.Position(call.Pos()).Line)
		if hasRetry {
			retrySites = append(retrySites, name)
		} else {
			plainSites = append(plainSites, name)
		}
		return true
	})
	if len(retrySites) != 6 || len(plainSites) != 2 {
		t.Fatalf("the ErrProvisionDiverged wrap table moved: %d retry-convergent site(s) (%v), %d deterministic site(s) (%v).\n"+
			"The retry-convergent wraps are spared by the approval arm (the retry lands); the deterministic ones are consumed (only a re-submission resolves them). "+
			"Say which side each wrap is on here and re-check deterministicProvisionRefusal and its witnesses.",
			len(retrySites), retrySites, len(plainSites), plainSites)
	}
}
