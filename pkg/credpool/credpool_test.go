package credpool

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestAudienceAllows(t *testing.T) {
	const (
		poolOrg  = "org-pool"
		otherOrg = "org-other"
	)
	cases := []struct {
		name    string
		aud     Audience
		orgID   string
		teamID  string
		userID  string
		isDonor bool
		want    bool
	}{
		{
			// The zero audience is the strictest useful policy, and the
			// default a freshly created pool carries.
			name: "zero audience serves the owning org only",
			aud:  Audience{}, orgID: poolOrg, teamID: "t1", want: true,
		},
		{
			name: "zero audience refuses a foreign org",
			aud:  Audience{}, orgID: otherOrg, teamID: "t1", want: false,
		},
		{
			name: "explicit team allow-list",
			aud:  Audience{Teams: []string{"t9"}}, orgID: otherOrg, teamID: "t9", want: true,
		},
		{
			name: "explicit team allow-list refuses another team",
			aud:  Audience{Teams: []string{"t9"}}, orgID: otherOrg, teamID: "t8", want: false,
		},
		{
			name: "org allow-list",
			aud:  Audience{Orgs: []string{otherOrg}}, orgID: otherOrg, teamID: "t8", want: true,
		},
		{
			name: "reciprocity admits an active donor from anywhere",
			aud:  Audience{Contributors: true}, orgID: otherOrg, teamID: "t8", userID: "u1", isDonor: true, want: true,
		},
		{
			name: "reciprocity refuses a non-donor",
			aud:  Audience{Contributors: true}, orgID: otherOrg, teamID: "t8", userID: "u1", isDonor: false, want: false,
		},
		{
			name: "all-teams admits everyone",
			aud:  Audience{AllTeams: true}, orgID: otherOrg, teamID: "t8", want: true,
		},
		{
			// The predicates are a union: any one of them admitting is
			// enough, which is what makes the policy composable.
			name: "union: team list admits even when the org does not",
			aud:  Audience{Orgs: []string{"org-nope"}, Teams: []string{"t8"}}, orgID: otherOrg, teamID: "t8", want: true,
		},
		{
			// An empty org id must not accidentally match an empty entry.
			name: "empty ids never match",
			aud:  Audience{Orgs: []string{""}, Teams: []string{""}}, orgID: "", teamID: "", want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.aud.Allows(poolOrg, tc.orgID, tc.teamID, tc.userID, tc.isDonor)
			if got != tc.want {
				t.Errorf("Allows = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAudienceNeedsPledgeLookup(t *testing.T) {
	if (Audience{}).NeedsPledgeLookup() {
		t.Error("the default audience must not cost a pledge lookup on every launch")
	}
	if !(Audience{Contributors: true}).NeedsPledgeLookup() {
		t.Error("reciprocity needs the donor lookup")
	}
}

func TestWindowOpen(t *testing.T) {
	// 2026-08-03 is a Monday.
	at := func(h int) time.Time { return time.Date(2026, 8, 3, h, 30, 0, 0, time.UTC) }

	cases := []struct {
		name string
		w    *Window
		now  time.Time
		want bool
	}{
		{"nil window is always open", nil, at(3), true},
		{"equal hours mean all day", &Window{StartHour: 9, EndHour: 9}, at(3), true},
		{"inside a daytime range", &Window{StartHour: 9, EndHour: 18}, at(12), true},
		{"before a daytime range", &Window{StartHour: 9, EndHour: 18}, at(8), false},
		{"end hour is exclusive", &Window{StartHour: 9, EndHour: 18}, at(18), false},
		// The case donors actually ask for: lend it overnight.
		{"wraps midnight, late evening", &Window{StartHour: 19, EndHour: 8}, at(23), true},
		{"wraps midnight, early morning", &Window{StartHour: 19, EndHour: 8}, at(2), true},
		{"wraps midnight, closed midday", &Window{StartHour: 19, EndHour: 8}, at(12), false},
		{"weekday match", &Window{Weekdays: []int{int(time.Monday)}}, at(12), true},
		{"weekday mismatch", &Window{Weekdays: []int{int(time.Sunday)}}, at(12), false},
		{
			// A typo in the timezone must not silently withdraw a donation.
			"unknown timezone falls back to UTC rather than closing",
			&Window{Timezone: "Mars/Olympus", StartHour: 9, EndHour: 18}, at(12), true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Open(tc.now); got != tc.want {
				t.Errorf("Open = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowOpen_honoursTimezone(t *testing.T) {
	// 06:30 UTC is 08:30 in Paris (CEST) — inside a 8→18 local window,
	// outside it if the window were read as UTC.
	w := &Window{Timezone: "Europe/Paris", StartHour: 8, EndHour: 18}
	if !w.Open(time.Date(2026, 8, 3, 6, 30, 0, 0, time.UTC)) {
		t.Error("window must be evaluated in the donor's timezone")
	}
}

func TestLimitsValidate(t *testing.T) {
	if err := (Limits{MaxUSDPerDay: 5, MaxUSDPerWeek: 20}).Validate(); err != nil {
		t.Errorf("valid limits rejected: %v", err)
	}
	if err := (Limits{MaxUSDPerDay: -1}).Validate(); err == nil {
		t.Error("negative spend cap accepted")
	}
	// A weekly cap under the daily one makes the daily figure a lie — the
	// donor would see "$10/day" and get $3 on the first day only.
	if err := (Limits{MaxUSDPerDay: 10, MaxUSDPerWeek: 3}).Validate(); err == nil {
		t.Error("weekly cap below the daily cap accepted")
	}
}

func TestPledgeAvailable(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	base := Pledge{Enabled: true, Health: HealthOK}

	cases := []struct {
		name   string
		mutate func(*Pledge)
		botID  string
		wantOK bool
		want   Status
	}{
		{"healthy and enabled", func(*Pledge) {}, "", true, StatusActive},
		{"kill switch", func(p *Pledge) { p.Enabled = false }, "", false, StatusPaused},
		{"unhealthy token", func(p *Pledge) { p.Health = HealthTokenExpired }, "", false, StatusUnhealthy},
		{
			"inside a provider cooldown",
			func(p *Pledge) { t := now.Add(time.Hour); p.CooldownUntil = &t },
			"", false, StatusCooling,
		},
		{
			// An elapsed cooldown must free the donor with no background
			// job: availability is derived, never stored.
			"elapsed cooldown is available again",
			func(p *Pledge) { t := now.Add(-time.Minute); p.CooldownUntil = &t },
			"", true, StatusActive,
		},
		{
			"outside the sharing window",
			func(p *Pledge) { p.Window = &Window{StartHour: 19, EndHour: 8} },
			"", false, StatusOutOfHours,
		},
		{
			"bot allow-list match",
			func(p *Pledge) { p.Bots = []string{"docs-refresh"} },
			"docs-refresh", true, StatusActive,
		},
		{
			// Distinct from paused: the donor IS sharing, just not with
			// this bot. Telling them "paused" would be a lie about their
			// own contribution.
			"bot outside the allow-list",
			func(p *Pledge) { p.Bots = []string{"docs-refresh"} },
			"feature-dev", false, StatusBotFiltered,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			ok, status := p.Available(now, tc.botID)
			if ok != tc.wantOK || status != tc.want {
				t.Errorf("Available = (%v, %s), want (%v, %s)", ok, status, tc.wantOK, tc.want)
			}
		})
	}
}

func TestRemainingAllowance(t *testing.T) {
	cases := []struct {
		name       string
		lim        Limits
		day, week  float64
		wantRem    float64
		wantCapped bool
	}{
		{"no cap at all", Limits{}, 3, 10, 0, false},
		{"daily cap only", Limits{MaxUSDPerDay: 5}, 2, 0, 3, true},
		{
			// The tightest cap must win: a nearly-spent week bounds today
			// even when the daily figure looks generous.
			"weekly cap is tighter than daily",
			Limits{MaxUSDPerDay: 5, MaxUSDPerWeek: 20}, 1, 19, 1, true,
		},
		{"daily cap is tighter than weekly", Limits{MaxUSDPerDay: 5, MaxUSDPerWeek: 100}, 4, 10, 1, true},
		{"overspend clamps to zero, never negative", Limits{MaxUSDPerDay: 5}, 7, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rem, capped := remainingAllowance(tc.lim, tc.day, tc.week)
			if rem != tc.wantRem || capped != tc.wantCapped {
				t.Errorf("remainingAllowance = (%v, %v), want (%v, %v)", rem, capped, tc.wantRem, tc.wantCapped)
			}
		})
	}
}

func TestPeriodKeys(t *testing.T) {
	// A week bucket must not roll over mid-week: Monday and the following
	// Sunday belong to the same ISO week.
	mon := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	sun := time.Date(2026, 8, 9, 23, 59, 0, 0, time.UTC)
	if weekKey(mon) != weekKey(sun) {
		t.Errorf("Monday %s and Sunday %s landed in different weeks", weekKey(mon), weekKey(sun))
	}
	if weekKey(sun.Add(time.Minute)) == weekKey(sun) {
		t.Error("the next Monday must open a new week bucket")
	}
	if got := weekStart(sun); !got.Equal(mon) {
		t.Errorf("weekStart = %s, want the Monday %s", got, mon)
	}
	if dayKey(mon) == dayKey(sun) {
		t.Error("distinct days must bucket distinctly")
	}
}

func TestCostToMillis(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0, 0}, {-1, 0}, {1, 1000}, {0.4242, 424}, {0.0004, 0}, {2.9995, 3000},
	}
	for _, tc := range cases {
		if got := CostToMillis(tc.in); got != tc.want {
			t.Errorf("CostToMillis(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPoolValidate(t *testing.T) {
	cases := []struct {
		name    string
		pool    Pool
		wantErr bool
	}{
		{
			// The zero values are the legacy row: an org-owned, audience
			// open pool. Every pre-ADR-164 pool validates untouched.
			name:    "legacy row (empty owner, empty visibility)",
			pool:    Pool{ID: "p1", OrgID: "org1", Enabled: true},
			wantErr: false,
		},
		{name: "explicit org owner", pool: Pool{ID: "p1", OrgID: "org1", OwnerType: PoolOwnerOrg, OwnerID: "org1", Enabled: true}},
		{name: "org owner not its own org", pool: Pool{ID: "p1", OrgID: "org1", OwnerType: PoolOwnerOrg, OwnerID: "org2", Enabled: true}, wantErr: true},
		{name: "user pool without an owner id", pool: Pool{ID: "p1", OwnerType: PoolOwnerUser, Enabled: true}, wantErr: true},
		{
			// The Audience org-implicit rule admits the owning org's teams —
			// a user pool carrying an OrgID would hand a private pool to a
			// whole org through that rule, contradicting owner-only draws.
			name:    "user pool carrying an OrgID",
			pool:    Pool{ID: "p1", OrgID: "org1", OwnerType: PoolOwnerUser, OwnerID: "u1", Enabled: true},
			wantErr: true,
		},
		{name: "platform pool", pool: Pool{ID: "p1", OwnerType: PoolOwnerPlatform, Enabled: true}},
		{name: "platform pool carrying an OrgID", pool: Pool{ID: "p1", OrgID: "org1", OwnerType: PoolOwnerPlatform, Enabled: true}, wantErr: true},
		{name: "unknown owner type", pool: Pool{ID: "p1", OwnerType: "team", Enabled: true}, wantErr: true},
		{name: "unknown visibility", pool: Pool{ID: "p1", OrgID: "org1", Visibility: "secret", Enabled: true}, wantErr: true},
		{
			// Private is a user-only shape: an org pool narrows with a zero
			// Audience instead (the zero narrows to its own org), which keeps
			// the org-implicit rule away from private pools entirely.
			name:    "private org pool",
			pool:    Pool{ID: "p1", OrgID: "org1", Visibility: PoolVisibilityPrivate, Enabled: true},
			wantErr: true,
		},
		{name: "private user pool", pool: Pool{ID: "p1", OwnerType: PoolOwnerUser, OwnerID: "u1", Visibility: PoolVisibilityPrivate, Enabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pool.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPoolOwnedBy(t *testing.T) {
	orgPool := Pool{ID: "p1", OrgID: "org1", Enabled: true}
	userPool := Pool{ID: "p2", OwnerType: PoolOwnerUser, OwnerID: "u1", Visibility: PoolVisibilityPrivate, Enabled: true}
	platformPool := Pool{ID: "p3", OwnerType: PoolOwnerPlatform, Enabled: true}
	if !orgPool.OwnedBy("", "org1") || !orgPool.OwnedBy(PoolOwnerOrg, "org1") {
		t.Error("a legacy org pool is owned by its org, with or without the explicit owner type")
	}
	if orgPool.OwnedBy(PoolOwnerOrg, "org2") || orgPool.OwnedBy(PoolOwnerUser, "u1") {
		t.Error("a foreign org, or a user, must not own an org pool")
	}
	if !userPool.OwnedBy(PoolOwnerUser, "u1") {
		t.Error("a user pool is owned by its user")
	}
	if userPool.OwnedBy(PoolOwnerUser, "u2") || userPool.OwnedBy(PoolOwnerOrg, "org1") {
		t.Error("nobody else owns a private user pool")
	}
	if !platformPool.OwnedBy(PoolOwnerPlatform, "") {
		t.Error("the platform owns a platform pool")
	}
	if platformPool.OwnedBy(PoolOwnerOrg, "org1") {
		t.Error("an org does not own the platform pool")
	}
}

func TestPledgeValidate_routeBinding(t *testing.T) {
	valid := Pledge{
		UserID:     "u1",
		Enabled:    true,
		Credential: Credential{Source: SourceAPIKey, Ref: "anthropic", KeyID: "k1"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid vendor-key pledge rejected: %v", err)
	}
	gateway := valid
	gateway.Credential = Credential{Source: SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible), KeyID: "k2"}
	if err := gateway.Validate(); err == nil {
		t.Error("a gateway entry without a model id accepted — its fallback rung could never compose")
	}
	gateway.Model = "openai_compatible/scaleway/deepseek-v4.1-flash"
	if err := gateway.Validate(); err != nil {
		t.Errorf("valid gateway entry rejected: %v", err)
	}
	gateway.Model = "deepseek v4.1"
	if err := gateway.Validate(); err == nil {
		t.Error("a model id with whitespace accepted — ids are verbatim (ADR-122)")
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, Backend: "claude code"}).Validate(); err == nil {
		t.Error("a backend with whitespace accepted")
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, Priority: -1}).Validate(); err == nil {
		t.Error("a negative priority accepted")
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, LendScope: "team:abc"}).Validate(); err == nil {
		t.Error("a foreign lend scope accepted")
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, LendScope: "orgtier:"}).Validate(); err == nil {
		t.Error("an orgtier scope naming no org accepted")
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, LendScope: secrets.OrgTierTenantID("org9")}).Validate(); err != nil {
		t.Errorf("valid orgtier lend scope rejected: %v", err)
	}
	if err := (Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, LendScope: secrets.PlatformTenantID}).Validate(); err != nil {
		t.Errorf("valid platform lend scope rejected: %v", err)
	}
	// A subscription entry may carry a model override without a backend.
	sub := Pledge{UserID: "u1", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, Model: "claude-sonnet-4-5"}
	if err := sub.Validate(); err != nil {
		t.Errorf("a subscription entry with a model override rejected: %v", err)
	}
}
