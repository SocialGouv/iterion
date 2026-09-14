package natsconfig

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func profileFixture(t *testing.T, accounts string) *Result {
	t.Helper()
	result, err := parseFixture(t.Context(), Sources{Entry: "main.conf", Files: map[string]string{
		"main.conf":     "VAR_CLIENT_PORT = 4222\nport: $VAR_CLIENT_PORT\njetstream: true\nsystem_account: SYS\ninclude \"accounts.conf\"\n",
		"accounts.conf": accounts,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNATSProfileProjectsStaticEffectivePermissions(t *testing.T) {
	key, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	public, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	result := profileFixture(t, fmt.Sprintf(`accounts {
  SYS { users: [{user: sys, password: sys-secret, permissions: {publish: ["$SYS.REQ.>"], subscribe: ["$SYS._INBOX.>"]}}] }
  WORK {
    default_permissions: {publish: {allow: ["safe.>"], deny: ["safe.blocked.>"]}, subscribe: ["safe.>"]}
    users: [
      {user: inherited, password: inherited-secret},
      {user: override, password: override-secret, permissions: {}},
      {nkey: "%s", permissions: {publish: ["safe.signed"], subscribe: ["safe.signed"]}}
    ]
  }
}`, public))
	profile, err := LoadProfile(result)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ParserVersion != ParserVersion || profile.ConfigDigest != result.Digest || profile.SystemAccount != "SYS" ||
		!slices.Equal(profile.Accounts, []string{"SYS", "WORK"}) || len(profile.Principals) != 4 {
		t.Fatalf("incorrect static profile: %+v", profile)
	}
	find := func(identity string) Principal {
		t.Helper()
		for _, principal := range profile.Principals {
			if principal.Identity == identity {
				return principal
			}
		}
		t.Fatalf("missing principal %s", identity)
		return Principal{}
	}
	inherited := find("inherited")
	if inherited.Kind != "password" || inherited.Account != "WORK" ||
		!slices.Equal(inherited.Publish.Allow, []string{"safe.>"}) ||
		!slices.Equal(inherited.Publish.Deny, []string{"safe.blocked.>"}) ||
		!slices.Equal(inherited.Subscribe.Allow, []string{"safe.>"}) {
		t.Fatalf("account defaults were not inherited: %+v", inherited)
	}
	if allowed, err := inherited.Publish.Allows("safe.blocked.one"); err != nil || allowed {
		t.Fatalf("deny lost under inherited allow: %t %v", allowed, err)
	}
	override := find("override")
	if allowed, err := override.Publish.Allows("$JS.API.CONSUMER.MSG.NEXT.ITERION_RUNS.iterion-runners"); err != nil || !allowed {
		t.Fatalf("explicit empty user permission did not override restrictive defaults: %t %v", allowed, err)
	}
	if signed := find(public); signed.Kind != "nkey" || !slices.Equal(signed.Publish.Allow, []string{"safe.signed"}) {
		t.Fatalf("public nkey user was not projected: %+v", signed)
	}
	encoded, err := json.Marshal(profile)
	if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "password:") {
		t.Fatalf("credential material leaked through profile projection: %v", err)
	}
}

func TestNATSProfileRejectsUnsupportedAuthorizationPaths(t *testing.T) {
	base := `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {users: [{user: worker, password: worker-secret}]} }`
	for name, extra := range map[string]string{
		"authorization token": `authorization: {token: "do-not-log-token"}`,
		"anonymous user":      `no_auth_user: worker`,
		"auth callout":        `authorization: {auth_callout: {issuer: "unsupported"}}`,
		"operator resolver":   `resolver: MEMORY`,
		"operator JWT":        `operator: "unsupported"`,
		"leaf node":           `leafnodes: {port: 7422}`,
		"gateway":             `gateway: {name: G, port: 7522}`,
		"websocket":           `websocket: {port: 9222}`,
		"mqtt":                `mqtt: {port: 1883}`,
		"subject mapping":     `mappings: {"safe.*": "unsafe.*"}`,
		"cluster":             `cluster: {port: 6222}`,
	} {
		t.Run(name, func(t *testing.T) {
			result := profileFixture(t, base+"\n"+extra)
			if _, err := LoadProfile(result); err == nil || strings.Contains(err.Error(), "do-not-log-token") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsupported NATS auth path accepted or leaked a secret: %v", err)
			}
		})
	}
	for name, accounts := range map[string]string{
		"account imports":           `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {imports: [{stream: "safe.>", account: SYS}], users: [{user: worker, password: worker-secret}]} }`,
		"account exports":           `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {exports: [{stream: "safe.>"}], users: [{user: worker, password: worker-secret}]} }`,
		"dynamic responses":         `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {users: [{user: worker, password: worker-secret, permissions: {allow_responses: true}}]} }`,
		"queue-qualified subscribe": `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {users: [{user: worker, password: worker-secret, permissions: {subscribe: ["safe.> workers"]}}]} }`,
	} {
		t.Run(name, func(t *testing.T) {
			result := profileFixture(t, accounts)
			if _, err := LoadProfile(result); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsupported account behavior accepted or leaked a secret: %v", err)
			}
		})
	}
}

func TestNATSProfileRejectsAmbiguousOrMalformedEffectiveIdentity(t *testing.T) {
	base := profileFixture(t, `accounts { SYS {users: [{user: sys, password: sys-secret}]}; WORK {users: [{user: worker, password: worker-secret}]} }`)
	for name, accounts := range map[string]string{
		"duplicate principal":           `{"SYS":{"users":[{"user":"same","password":"one"}]},"WORK":{"users":[{"user":"same","password":"two"}]}}`,
		"username aliases":              `{"SYS":{"users":[{"user":"sys","username":"other","password":"one"}]},"WORK":{"users":[{"user":"worker","password":"two"}]}}`,
		"password aliases":              `{"SYS":{"users":[{"user":"sys","password":"one","pass":"two"}]},"WORK":{"users":[{"user":"worker","password":"three"}]}}`,
		"typed identity":                `{"SYS":{"users":[{"user":42,"username":"sys","password":"one"}]},"WORK":{"users":[{"user":"worker","password":"two"}]}}`,
		"unknown account key":           `{"SYS":{"users":[{"user":"sys","password":"one"}]},"WORK":{"mappings":{},"users":[{"user":"worker","password":"two"}]}}`,
		"unknown user key":              `{"SYS":{"users":[{"user":"sys","password":"one"}]},"WORK":{"users":[{"user":"worker","password":"two","proxy_required":true}]}}`,
		"unaddressable census identity": `{"SYS":{"users":[{"user":"sys","password":"one"}]},"WORK":{"users":[{"user":"worker.other","password":"two"}]}}`,
		"unknown permission key":        `{"SYS":{"users":[{"user":"sys","password":"one"}]},"WORK":{"users":[{"user":"worker","password":"two","permissions":{"allow_responses":true}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *base
			candidate.Config = make(map[string]json.RawMessage, len(base.Config))
			for key, value := range base.Config {
				candidate.Config[key] = value
			}
			candidate.Config["accounts"] = json.RawMessage(accounts)
			if _, err := LoadProfile(&candidate); err == nil {
				t.Fatal("malformed effective identity accepted")
			}
		})
	}
	badVariable := *base
	badVariable.Variables = []string{"PORT"}
	badVariable.Config = make(map[string]json.RawMessage, len(base.Config)+1)
	for key, value := range base.Config {
		badVariable.Config[key] = value
	}
	badVariable.Config["PORT"] = json.RawMessage(`4222`)
	if _, err := LoadProfile(&badVariable); err == nil {
		t.Fatal("operational-key variable collision accepted")
	}
	future := *base
	future.ParserVersion = "2.14.6"
	if _, err := LoadProfile(&future); err == nil {
		t.Fatal("unvalidated broker/parser version accepted")
	}
	foreignDomain := *base
	foreignDomain.Config = make(map[string]json.RawMessage, len(base.Config))
	for key, value := range base.Config {
		foreignDomain.Config[key] = value
	}
	foreignDomain.Config["jetstream"] = json.RawMessage(`{"domain":"OTHER"}`)
	if _, err := LoadProfile(&foreignDomain); err == nil {
		t.Fatal("unreviewed JetStream API domain accepted")
	}
}
