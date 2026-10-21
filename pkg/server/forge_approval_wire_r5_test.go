package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The org-approval decision surface must carry the SPOKEN-EMPTY gestures
// the gate parks BY NAME (expandsProvisionSurface): a hold_labels [] lift,
// a label_allowlist [] widen, a launch_vars {} clear. The record's json
// tags keep omitempty on the three fields, so an empty value collapses to
// an ABSENT key and the approver's card reads the record as silent while
// approve executes the gesture. Each spoken field therefore travels with
// a <field>_set fact (the RoutingSet precedent) so the wire tells a
// spoken-empty from a silent record.

func parkUpdate(t *testing.T, s *Server, integrationID, body string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+integrationID, body, "t1")
	req.SetPathValue("integration_id", integrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("update should park: code=%d body=%s", w.Code, w.Body.String())
	}
	return approvalIDFrom(t, w)
}

func orgApprovalWire(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleListOrgProvisionApprovals(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/provision-approvals", "", "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org list: code=%d body=%s", w.Code, w.Body.String())
	}
	var lr struct {
		Approvals []json.RawMessage `json:"approvals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatalf("org list decode: %v", err)
	}
	if len(lr.Approvals) != 1 {
		t.Fatalf("want exactly one parked approval, got %d: %s", len(lr.Approvals), w.Body.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(lr.Approvals[0], &rec); err != nil {
		t.Fatalf("approval decode: %v", err)
	}
	return rec, string(lr.Approvals[0])
}

func approveRecord(t *testing.T, s *Server, id string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+id+"/approve", "", "o1")
	req.SetPathValue("approval_id", id)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
}

// seedIntegration provisions one repo directly as the org admin (the gate
// skips org admins) with the given extra settings JSON folded into the
// enable body.
func seedIntegration(t *testing.T, s *Server, connID, repo, extras string) forge.ProvisionResult {
	t.Helper()
	body := `{"connection_id":"` + connID + `","repo":"` + repo + `","bot_ids":["review-pr"]` + extras + `}`
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.IntegrationID == "" {
		t.Fatalf("seed enable result: %s (%v)", w.Body.String(), err)
	}
	return res
}

func TestRV_R5Wire_SpokenEmptyGestures_RideTheOrgWire(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api",
		`,"launch_vars":{"gate_context":"api"},"auto_fix_on_gate_failure":true,`+
			`"hold_labels":["do-not-run"],"label_allowlist":["implement"]`)
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.HoldLabels) != 1 || len(ri.LabelAllowlist) != 1 {
		t.Fatalf("seed state: %+v (%v)", ri, err)
	}

	// ---- Phase 1: the hold-label LIFT, spoken as []. ----
	id := parkUpdate(t, s, res.IntegrationID, `{"hold_labels":[]}`)
	rec, raw := orgApprovalWire(t, s)
	// The gesture itself rides the wire: an empty-but-PRESENT slice (a
	// silent record marshals null), so a record parked before the Set
	// flags shipped still reads as what it is.
	if v, ok := rec["hold_labels"].([]any); !ok || len(v) != 0 {
		t.Fatalf("the spoken empty hold_labels slice must ride the org wire as [] (null = silent): %s", raw)
	}
	// THE FACT: the wire names the field as SPOKEN, so the card can render
	// the lift the approval executes.
	if rec["hold_labels_set"] != true {
		t.Fatalf("spoken hold_labels [] is invisible on the org wire (no hold_labels_set fact): %s", raw)
	}
	approveRecord(t, s, id)
	ri, err = s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.HoldLabels) != 0 {
		t.Fatalf("approve did not execute the lift: %+v (%v)", ri.HoldLabels, err)
	}

	// ---- Phase 2: the allowlist WIDEN, spoken as []. ----
	id = parkUpdate(t, s, res.IntegrationID, `{"label_allowlist":[]}`)
	rec, raw = orgApprovalWire(t, s)
	if v, ok := rec["label_allowlist"].([]any); !ok || len(v) != 0 {
		t.Fatalf("the spoken empty label_allowlist slice must ride the org wire as [] (null = silent): %s", raw)
	}
	if rec["label_allowlist_set"] != true {
		t.Fatalf("spoken label_allowlist [] is invisible on the org wire (no label_allowlist_set fact): %s", raw)
	}
	approveRecord(t, s, id)
	ri, err = s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.LabelAllowlist) != 0 {
		t.Fatalf("approve did not execute the widen: %+v (%v)", ri.LabelAllowlist, err)
	}

	// ---- Phase 3: a SILENT record carries no Set fact — the wire tells
	// a spoken-empty from a silent request. The schedule change is what
	// parks it; the three spoken-empty fields stay unspoken. ----
	id = parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["review-pr"],"schedule_crons":{"review-pr":"0 3 * * *"}}`)
	rec, raw = orgApprovalWire(t, s)
	if rec["hold_labels_set"] == true || rec["label_allowlist_set"] == true || rec["launch_vars_set"] == true {
		t.Fatalf("silent record carries a spoken fact: %s", raw)
	}
	approveRecord(t, s, id)
}

func TestRV_R5Wire_ReplaceTrue_WireTellsSilentFromClear(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)

	// The card-lie shape: launch_vars_replace spoken, NO map. The server
	// treats it as SILENT (a nil map keeps the stored pins — only a
	// non-nil map is a write), so approval KEEPS gate_context — while the
	// wire must let the card say exactly that instead of promising a clear.
	w := httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"],"launch_vars_replace":true}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	rec, raw := orgApprovalWire(t, s)
	if rec["launch_vars_replace"] != true {
		t.Fatalf("precondition drifted: launch_vars_replace not on the wire: %s", raw)
	}
	// No map was spoken, so the Set fact is ABSENT — the fact the card's
	// "no map spoken — the stored pins stay" branch reads. The contrast
	// with the spoken-clear record below is what tells the two apart.
	if rec["launch_vars_set"] == true {
		t.Fatalf("silent record claims a spoken map: %s", raw)
	}
	approveRecord(t, s, rec["id"].(string))
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || ri.LaunchVars["gate_context"] != "api" {
		t.Fatalf("silent replace must keep the stored pins: %+v (%v)", ri.LaunchVars, err)
	}

	// The contrast: a SPOKEN empty map ({} + replace) IS a whole-map
	// clear server-side. Pre-fix the two records were byte-identical to
	// the approver — the same absent launch_vars key, the same
	// launch_vars_replace — so the card promised a clear for both. With
	// the Set fact the wire separates them, and the clear still executes.
	w = httptest.NewRecorder()
	body = `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"],"launch_vars":{},"launch_vars_replace":true}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("clear enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	rec, raw = orgApprovalWire(t, s)
	if rec["launch_vars_set"] != true {
		t.Fatalf("spoken {} clear is invisible on the org wire (no launch_vars_set fact): %s", raw)
	}
	approveRecord(t, s, rec["id"].(string))
	ri, err = s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.LaunchVars) != 0 {
		t.Fatalf("spoken clear did not execute: %+v (%v)", ri.LaunchVars, err)
	}
}

// The enable route parks with auto_fix_on_gate_failure:false (a tightening
// the approver executes sight-unseen): the wire must carry auto_fix:false
// itself — a *bool needs no Set fact, false already distinguishes spoken
// from silent — so the card can render "auto-fix OFF".
func TestRV_R5Wire_AutoFixFalse_RidesTheOrgWire(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"],"auto_fix_on_gate_failure":false}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	_, raw := orgApprovalWire(t, s)
	if !strings.Contains(raw, `"auto_fix":false`) {
		t.Fatalf("auto_fix:false collapsed off the org wire: %s", raw)
	}
}
