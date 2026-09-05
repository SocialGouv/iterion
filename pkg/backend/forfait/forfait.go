// Package forfait implements a best-effort Anthropic "forfait" (Claude Code
// OAuth subscription) usage-cap check used by the LAYER-2 run-level
// auto-resume loop.
//
// When an auto-resume would drive the run against the Claude Code OAuth
// forfait, blindly re-launching into a near-exhausted 5-hour or 7-day window
// just burns attempts against a wall that won't move for hours. This package
// reads the current utilization from Anthropic's OAuth usage endpoint and
// lets the caller STOP (staying failed_resumable with a legible message)
// instead of looping.
//
// It is strictly best-effort: every failure to determine usage (no OAuth
// token, an API key is in use instead of the forfait, the endpoint is
// unreachable, a malformed body) degrades to Skipped=true — the caller then
// proceeds by attempt-count only and NEVER blocks on this check. The one
// thing it must never do is leak the OAuth access token: the token is read,
// used as a Bearer header, and never logged or returned.
package forfait

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultCapPct is the utilization percentage at or above which an
// auto-resume against the forfait is withheld. Overridable via
// ITERION_FORFAIT_CAP_PCT.
const DefaultCapPct = 85.0

// usageEndpoint is the Anthropic OAuth usage endpoint. Returns
// {five_hour:{utilization}, seven_day:{utilization}} as percentages.
const usageEndpoint = "https://api.anthropic.com/api/oauth/usage"

// oauthBetaHeader is the required beta opt-in for the OAuth usage endpoint.
const oauthBetaHeader = "oauth-2025-04-20"

// Usage is the forfait utilization snapshot, as percentages in [0, 100].
type Usage struct {
	FiveHour float64
	SevenDay float64
}

// Decision is the outcome of a cap check.
type Decision struct {
	// Blocked reports that at least one window is at/over the cap, so the
	// caller should NOT auto-resume.
	Blocked bool
	// Skipped reports that usage could not be determined (best-effort): no
	// token, not a forfait run, endpoint unreachable, malformed body, or the
	// check is disabled. The caller proceeds by attempt-count only.
	Skipped bool
	// Reason is a human-readable, token-free explanation for logs / the
	// failed_resumable message.
	Reason string
	// Usage is the snapshot that drove the decision (nil when Skipped for a
	// reason other than "over cap").
	Usage *Usage
}

// Doer is the minimal HTTP surface, satisfied by *http.Client. Injectable
// so tests can stub the transport.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// CapPctFromEnv resolves the utilization cap from ITERION_FORFAIT_CAP_PCT,
// falling back to DefaultCapPct. A value <= 0 disables the check (the caller
// treats a non-positive cap as "skip"); a malformed value keeps the default.
func CapPctFromEnv() float64 {
	raw := strings.TrimSpace(os.Getenv("ITERION_FORFAIT_CAP_PCT"))
	if raw == "" {
		return DefaultCapPct
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return DefaultCapPct
	}
	return v
}

// Decide is the pure cap decision: Blocked when either window is at/over the
// cap. Kept separate from the I/O so it is trivially unit-testable.
func Decide(u Usage, capPct float64) Decision {
	if u.FiveHour >= capPct || u.SevenDay >= capPct {
		return Decision{
			Blocked: true,
			Usage:   &u,
			Reason: fmt.Sprintf("forfait cap %.0f%% reached (5h=%.0f%%/7d=%.0f%%), resume later",
				capPct, u.FiveHour, u.SevenDay),
		}
	}
	return Decision{
		Usage: &u,
		Reason: fmt.Sprintf("forfait usage under cap %.0f%% (5h=%.0f%%/7d=%.0f%%)",
			capPct, u.FiveHour, u.SevenDay),
	}
}

// Check performs the full best-effort cap check with the real seams (env,
// ~/.claude/.credentials.json, the live HTTP endpoint). Any inability to
// determine usage returns Skipped=true so the caller never blocks on it.
func Check(ctx context.Context, capPct float64) Decision {
	return check(ctx, capPct, claudeConfigDir, os.Getenv("ANTHROPIC_API_KEY"),
		&http.Client{Timeout: 8 * time.Second})
}

// check is the injectable core of Check.
func check(ctx context.Context, capPct float64, configDir func() string, anthropicAPIKey string, doer Doer) Decision {
	if capPct <= 0 {
		return Decision{Skipped: true, Reason: "forfait cap check disabled (cap <= 0)"}
	}
	// An explicit API key means the run bills metered API calls, not the
	// forfait — the forfait cap does not apply. Skip by count.
	if strings.TrimSpace(anthropicAPIKey) != "" {
		return Decision{Skipped: true, Reason: "not a forfait run (ANTHROPIC_API_KEY set)"}
	}
	token, ok := readAccessToken(configDir)
	if !ok {
		return Decision{Skipped: true, Reason: "no Claude Code OAuth token found (not a forfait run)"}
	}
	u, err := fetchUsage(ctx, token, doer)
	if err != nil {
		// Best-effort: an unreachable / erroring usage endpoint must never
		// block an auto-resume. Degrade to count-only. The token is NOT in
		// err (fetchUsage does not embed it).
		return Decision{Skipped: true, Reason: "forfait usage unavailable: " + err.Error()}
	}
	return Decide(*u, capPct)
}

// WindowUsage is one provider window as the OAuth usage endpoint reports
// it — the same vocabulary as the CLI's rate_limit_event, so a caller can
// record it where session telemetry lands.
type WindowUsage struct {
	// Window is the provider's window name: five_hour, seven_day,
	// seven_day_opus, seven_day_sonnet.
	Window string
	// Utilization is the FRACTION consumed, 0..1 (the endpoint reports a
	// percentage; converted here so it matches the session telemetry).
	Utilization float64
	// ResetsAt is when the window rolls over; zero when the endpoint did
	// not say.
	ResetsAt time.Time
}

// usageWindows are the endpoint's per-window objects, in the order they
// are reported.
var usageWindows = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// FetchWindows GETs the OAuth usage endpoint with the given bearer token
// and returns every window it reports. The token is used only as the
// Authorization header — it is never included in a returned error. A
// non-200 is an error the caller degrades on: a `claude setup-token`
// credential lacks the user:profile scope the endpoint requires and is
// answered 403.
func FetchWindows(ctx context.Context, token string, doer Doer) ([]WindowUsage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}

	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode usage body: %w", err)
	}
	out := make([]WindowUsage, 0, len(usageWindows))
	for _, name := range usageWindows {
		raw, ok := body[name]
		if !ok || string(raw) == "null" {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("decode usage window %s: %w", name, err)
		}
		if w.Utilization == nil {
			continue
		}
		wu := WindowUsage{Window: name, Utilization: *w.Utilization / 100}
		if w.ResetsAt != "" {
			t, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
			if err != nil {
				return nil, fmt.Errorf("decode usage window %s: resets_at %q: %w", name, w.ResetsAt, err)
			}
			wu.ResetsAt = t.UTC()
		}
		out = append(out, wu)
	}
	return out, nil
}

// fetchUsage is the two-window percentage view the auto-resume cap check
// decides on, over FetchWindows.
func fetchUsage(ctx context.Context, token string, doer Doer) (*Usage, error) {
	windows, err := FetchWindows(ctx, token, doer)
	if err != nil {
		return nil, err
	}
	u := &Usage{}
	for _, w := range windows {
		switch w.Window {
		case "five_hour":
			u.FiveHour = w.Utilization * 100
		case "seven_day":
			u.SevenDay = w.Utilization * 100
		}
	}
	return u, nil
}

// readAccessToken reads the Claude Code OAuth access token from
// <configDir>/.credentials.json. Returns ("", false) on any problem — a
// missing file is the common "not a forfait host" case. The token value is
// never logged.
func readAccessToken(configDir func() string) (string, bool) {
	dir := configDir()
	if dir == "" {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return "", false
	}
	return AccessTokenFromCredentialsJSON(data)
}

// AccessTokenFromCredentialsJSON extracts the Claude Code OAuth access
// token from a credentials.json payload — the shape on disk under
// ~/.claude and the blob a cloud forfait is uploaded as. ("", false) on any
// problem; the token is never logged.
func AccessTokenFromCredentialsJSON(data []byte) (string, bool) {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", false
	}
	tok := strings.TrimSpace(creds.ClaudeAiOauth.AccessToken)
	if tok == "" {
		return "", false
	}
	return tok, true
}

// claudeConfigDir mirrors detect.claudeConfigDir without importing it (avoids
// a heavier dependency for a one-liner): CLAUDE_CONFIG_DIR, else ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}
