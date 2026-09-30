package e2e

import (
	"strings"
	"testing"
	"time"
)

// pwLabelDetail renders one Sentry issue through a sentry_detail label and
// returns the posted body.
func pwLabelDetail(t *testing.T, label, culprit string) string {
	t.Helper()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		cfg["labels"] = map[string]any{"sentry_detail": label}
	})
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4601", ShortID: strp("P-4601"), Title: "x", Culprit: culprit, FirstProcessed: now, LastSeen: now, Count: 1})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	return strings.Join(h.bodies()[n:], "\n")
}

// TestProdWatch_ALabelsHostAfterAnyLetterIsInert: the autolinker reads a host
// after any letter (santé.fr), a `-` or a `.`, wherever its url rule is tried
// — after an escaped mark, right after a code span: every dot of a label is
// escaped, so no host of the label's own words links through a value's span.
func TestProdWatch_ALabelsHostAfterAnyLetterIsInert(t *testing.T) {
	t.Parallel()
	for name, label := range map[string]string{
		"an accented host in emphasis": "{level} · *santé.fr/{culprit}*",
		"an accented host in a link":   "{level} · [santé.fr/{culprit}]",
		"a host after an underscore":   "{level} · logs_santé.fr/{culprit}",
		"a host after a hyphen":        "{level} · x-.co/{culprit}",
		"a host after dots":            "{level} · __..co/{culprit}",
		"a Cyrillic host":              "{level} · *пример.рф/{culprit}*",
	} {
		name, label := name, label
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := pwLabelDetail(t, label, "@channel see www.evil-sso.com/login")
			found := false
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, "`@channel see www.evil-sso.com/login`") {
					found = true
					if bad := pwUnescapedActives(line); len(bad) > 0 {
						t.Fatalf("%s: the label's words stay live around the value (%q):\n%s", name, bad, line)
					}
				}
			}
			if !found {
				t.Fatalf("%s: setup: the detail line with the value was not posted:\n%s", name, body)
			}
		})
	}
}

// TestProdWatch_ALabelsAreOneLine: a label with line breaks (a blank line opens
// a block: a table would split a value's code span at `|`) renders on one line.
func TestProdWatch_ALabelsAreOneLine(t *testing.T) {
	t.Parallel()
	body := pwLabelDetail(t, "\n\nCulprit | Level\n--- | ---\n{culprit} | {level}", "x | @channel see www.evil-sso.com/login")
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "Culprit") && !strings.Contains(line, "`x | @channel see www.evil-sso.com/login`") {
			t.Fatalf("the label's line breaks survived: its words and the value are on different lines:\n%s", body)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "---") {
			t.Fatalf("a label line opens a table:\n%s", body)
		}
	}
}

// TestProdWatch_ALabelsBlankValueJoinsTheWords: a value that renders as nothing
// (blank) joins the words around it — escaped as one text, they form no scheme
// (`https{culprit}://x` with a blank culprit).
func TestProdWatch_ALabelsBlankValueJoinsTheWords(t *testing.T) {
	t.Parallel()
	body := pwLabelDetail(t, "{level} · https{culprit}://x/{short_id}", "   ")
	if !strings.Contains(body, "https\u200b://x/") {
		t.Fatalf("the words around a blank value form a live scheme:\n%q", body)
	}
}

// TestProdWatch_AFoldLabelWithoutNamesIsRefused: a note stamps as said the
// members it names — a folded_detail (or folded_detail_more) override without
// the {names} placeholder would say nobody; plan refuses it by name.
func TestProdWatch_AFoldLabelWithoutNamesIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, key := range []string{"folded_detail", "folded_detail_more"} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) { cfg["labels"] = map[string]any{key: "{n} de plus ce tour-ci : {noms}"} })
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars,
				map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
			if err == nil || !strings.Contains(stderr, "labels."+key+" must keep the {names} placeholder") || strings.Contains(stderr, "Traceback") {
				t.Fatalf("a %s override without {names} was not refused by name: %v %s", key, err, lastN(stderr, 300))
			}
		})
	}
}
