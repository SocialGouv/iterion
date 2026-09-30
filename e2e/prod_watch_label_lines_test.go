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

// TestProdWatch_ALabelsAreOneLine: a label with line breaks — of any kind
// Mattermost reads as one (a blank line opens a block: a table would split a
// value's code span at `|`) — renders on one line.
func TestProdWatch_ALabelsAreOneLine(t *testing.T) {
	t.Parallel()
	for name, br := range map[string]string{"LF": "\n", "CR": "\r", "U+2424": "\u2424", "VT": "\v", "FF": "\f", "LS": "\u2028", "PS": "\u2029"} {
		name, br := name, br
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			label := br + br + "Culprit | Level" + br + "--- | ---" + br + "{culprit} | {level}"
			body := pwLabelDetail(t, label, "x | @channel see www.evil-sso.com/login")
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, "Culprit") && (!strings.Contains(line, "`x | @channel see www.evil-sso.com/login`") || strings.Contains(line, br)) {
					t.Fatalf("%s: the label's line breaks survived: its words and the value are not one line:\n%q", name, body)
				}
			}
		})
	}
}

// TestProdWatch_ALabelsMentionNobody: a label cannot tag — its own @channel,
// @here or @all, or an `@` a blank value joins to a word, reaches no one.
func TestProdWatch_ALabelsMentionNobody(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ label, culprit, want string }{
		"its own @channel": {"{level} · @channel look", "app.views", "@\u200bchannel"},
		"its own @here":    {"{level} · @here look {culprit}", "app.views", "@\u200bhere"},
		"a blank value":    {"{level} · @{culprit}here", "   ", "@\u200bhere"},
	} {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if body := pwLabelDetail(t, c.label, c.culprit); !strings.Contains(body, c.want) {
				t.Fatalf("%s: a live mention came out of the label:\n%q", name, body)
			}
		})
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
	labelFor := func(key string) string {
		switch {
		case strings.HasSuffix(key, "/twice"):
			return "{n} de plus : {names} (rappel : {names})"
		case strings.HasSuffix(key, "/long"):
			return "{n} de plus : {names} " + strings.Repeat("blabla ", 60)
		default:
			return "{n} de plus ce tour-ci : {noms}"
		}
	}
	for _, key := range []string{"folded_detail", "folded_detail_more", "folded_detail/twice", "folded_detail/long"} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) {
				cfg["labels"] = map[string]any{strings.SplitN(key, "/", 2)[0]: labelFor(key)}
			})
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars,
				map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
			if err == nil || !strings.Contains(stderr, "labels."+strings.SplitN(key, "/", 2)[0]+" must hold the {names} placeholder exactly once") ||
				strings.Contains(stderr, "Traceback") {
				t.Fatalf("a %s override was not refused by name: %v %s", key, err, lastN(stderr, 300))
			}
		})
	}
}
