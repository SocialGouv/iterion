package forge

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// provisionerOwnedIntegrationFields are the fields a provision legitimately
// decides: they come from the request, the resolved bots, or the forge round
// trip. Everything else on RepoIntegration is set through some other surface
// (today: PATCH /forge/integrations/{iid}) and must survive a re-provision.
//
// This list is the point of the sweep below. Adding a field to
// RepoIntegration without deciding which side it falls on now fails a test
// instead of being discovered on a live repo.
var provisionerOwnedIntegrationFields = map[string]string{
	"ID":                   "identity",
	"TenantID":             "identity",
	"ConnectionID":         "from the request",
	"Provider":             "from the connection",
	"RepoFullName":         "from the request",
	"BotIDs":               "the request's desired set",
	"EventsNormalized":     "derived from the bots' manifests",
	"WebhookID":            "the provisioner's own record",
	"HookID":               "returned by the forge",
	"HookURL":              "where the forge should deliver — the provisioner decides it",
	"ManagedSecretID":      "the provisioner's own record",
	"LaunchVars":           "operator override, explicitly re-resolved (nil request = keep)",
	"Overlap":              "operator override, explicitly re-resolved",
	"HoldLabels":           "operator override, explicitly re-resolved",
	"LabelAllowlist":       "operator override, explicitly re-resolved",
	"AutoFixOnGateFailure": "operator override, explicitly re-resolved",
	"CreatedBy":            "identity",
	"CreatedAt":            "identity",
	"UpdatedAt":            "stamped every write",
}

// TestReprovisionDropsNoIntegrationFieldItDoesNotOwn is the class guard.
//
// A provision rebuilds RepoIntegration from the REQUEST and Updates it, and
// the update replaces the whole document. Any field the literal omits is
// therefore erased — not with an error, but silently, on a live repo. That
// is how a re-provision came to wipe sync_issues_enabled (issue sync stops)
// and min_author_role (the trust threshold deciding triage:auto vs
// needs:approval relaxes back to the default).
//
// Rather than pin the three fields known to have been lost, this walks every
// field: anything that goes to zero must be named above with the reason it
// belongs to the provisioner.
func TestReprovisionDropsNoIntegrationFieldItDoesNotOwn(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// Fill every field that is still zero, so "went to zero" is meaningful
	// for all of them and a newly added field is covered without editing
	// this test.
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(&integ).Elem()
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() || !f.IsZero() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("sentinel")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.Append(f, reflect.ValueOf("sentinel").Convert(f.Type().Elem())))
		case reflect.Map:
			m := reflect.MakeMap(f.Type())
			m.SetMapIndex(reflect.ValueOf("k").Convert(f.Type().Key()),
				reflect.ValueOf("v").Convert(f.Type().Elem()))
			f.Set(m)
		case reflect.Struct:
			if f.Type() == reflect.TypeOf(time.Time{}) {
				f.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
			}
		}
	}
	// Keep the identity coherent so the re-provision finds this record.
	integ.TenantID, integ.ConnectionID, integ.RepoFullName = "t1", "conn-1", "group/api"
	if err := o.Integrations.Update(ctx, integ); err != nil {
		t.Fatal(err)
	}

	// Enabling one more bot, which is what takes the REBUILD path. Re-sending
	// the identical request would short-circuit and never reach the Update
	// this test is about — the sweep would pass while proving nothing.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr", "revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}

	av := reflect.ValueOf(after)
	for i := 0; i < av.NumField(); i++ {
		name := typ.Field(i).Name
		if !av.Field(i).IsZero() {
			continue
		}
		if why, ok := provisionerOwnedIntegrationFields[name]; ok {
			_ = why
			continue
		}
		t.Errorf("re-provisioning erased RepoIntegration.%s. It is not a field the "+
			"provisioner owns, so it was set through another surface and the operator "+
			"gets no error — only the behaviour it controlled, gone. Either carry it "+
			"beside SyncIssuesEnabled/LastSyncedAt/MinAuthorRole, or add it to "+
			"provisionerOwnedIntegrationFields with the reason.", name)
	}
}

// TestReprovisionKeepsIssueSyncAndItsTrustThreshold is the concrete case the
// sweep generalises, kept because it names the consequence: measured on a
// live repo, a re-provision turned sync_issues_enabled off and reset the
// sync watermark.
func TestReprovisionKeepsIssueSyncAndItsTrustThreshold(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	synced := time.Unix(1700000000, 0).UTC()
	integ.SyncIssuesEnabled = true
	integ.LastSyncedAt = synced
	integ.MinAuthorRole = "maintainer" // tightened above the "" default
	if err := o.Integrations.Update(ctx, integ); err != nil {
		t.Fatal(err)
	}

	// Enabling one more bot is not a request to change any of this.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr", "revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	after, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.SyncIssuesEnabled {
		t.Error("issue sync was switched off by a re-provision: the board stops receiving " +
			"this repo's issues, and nothing reports it")
	}
	if !after.LastSyncedAt.Equal(synced) {
		t.Errorf("the sync watermark was reset (%v → %v): the next pass re-scans from the "+
			"start, and 'when did this last sync' is gone", synced, after.LastSyncedAt)
	}
	if after.MinAuthorRole != "maintainer" {
		t.Errorf("the author-trust threshold was reset (%q → %q). It decides triage:auto vs "+
			"needs:approval, so this does not fail — it RELAXES the gate an operator "+
			"deliberately tightened", "maintainer", after.MinAuthorRole)
	}
}
