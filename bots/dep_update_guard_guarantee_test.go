package bots

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The guarantee test of gate continuity (#3128). Every other Vetty test pins
// one node against a stub shaped for it; this one drives the bot's REAL
// deterministic nodes — prepare, continuity, post_feedback (its repin
// included), feedback_health and arm_automerge, plus the compute nodes
// between them — through the compiled graph's own edges and with-mappings,
// across a sequence of heads of ONE pull request, against a forge that keeps
// state: what a publish posts is what the next run reads. Only the model
// nodes (and the probes that read what they did) are stubbed, and only on
// the runs that take the full audit; a run that the graph routes into a
// model node with no stub stops there, and that IS the full audit.
//
// The forge is GitHub as the bot sees it, for acme/widgets#7: the pull
// request; its reviews over GraphQL, with their edit record; commit statuses,
// one current per context, each with its creator; the check rollup with each
// context's isRequired; auto-merge (refused on CLEAN and UNSTABLE, as GitHub
// does, and fired by the forge once every required check passes); a merge
// pinned with expectedHeadOid; the iterion server's publish endpoint (one
// review, then the gate status — refused for a pin that is not the head) and
// its launch claim; and the git endpoint over TLS, served by git's own
// http-backend, so the repin's fetch carries the token the way production
// does. Every request is recorded, every script's output too, and every git
// argv through a shim.
//
// The assertions, numbered as the operator asked:
//
//  1. a pure rebase of a green head is carried with no model node on the
//     path, and the carried status is pinned to the new head;
//  2. a hold on ANY head of the same change vetoes every later carry and
//     every repin, whichever of two concurrent runs finishes first — and a
//     carry decided before the hold landed is withdrawn at publish time;
//  3. an App review edited after it was created (its marker deleted, its
//     origin_at rejuvenated, a marker appended) never yields a carry: it
//     vetoes;
//  4. a status another identity wrote (renovate[bot], a PAT user) is never a
//     source, even with the right stamp and target_url;
//  5. a change whose identity differs (a conflict-resolving rebase, a binary
//     then a mode change, an empty file) is never carried;
//  6. on a base whose ONLY required context is the gate, a carried head with
//     red or pending non-required CI is never merged nor armed, at the CLEAN
//     and the UNSTABLE door; with a required CI context green it merges,
//     pinned to the vouched head;
//  7. past carry_max_age_h from the ORIGINAL audit nothing is carried,
//     however many carries chained — nor published by a run resumed past it;
//  8. the forge token never appears in an argv nor in any recorded output.
//
// The mutants of the production bot that turn each one red are published
// with the lane (the commit body); each subtest names its own.
func TestDepUpdateGuardContinuityGuarantee_AdversarialForge(t *testing.T) {
	for _, tool := range []string{"python3", "git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}

	// 1. Mutants: route `continuity -> security_audit when carry` or give the
	// carry path a model node (the static walk and the drive stop short);
	// map the carry edge's audited_sha from continuity.origin_sha (the
	// carried status never lands on the new head).
	t.Run("1 a pure rebase is carried with no model node, pinned to the new head", func(t *testing.T) {
		w := newGtWorld(t, gtGate, gtCI)
		x := w.bump(w.base0, "key20: v99")
		w.push(x)
		w.f.setCI(x, "IN_PROGRESS", "")
		// The audit of X is in flight when the base moves and the pull
		// request is rebased: it finishes after the push.
		r0 := w.launch(x, nil)
		r0.wantStop(t, "security_audit")
		base1 := w.moveBase(w.base0, "two")
		y := w.bump(base1, "key20: v99")
		w.push(y)
		w.f.setCI(y, "IN_PROGRESS", "")
		r0.audit(t, "clean")
		// The repin (arm 1b) carries the audited green onto Y.
		w.wantCarried(t, y, x)
		w.wantArmedOn(t, y)

		base2 := w.moveBase(base1, "three")
		y2 := w.bump(base2, "key20: v99")
		w.push(y2)
		w.f.setCI(y2, "IN_PROGRESS", "")
		r := w.launch(y2, nil)
		if r.stop != "done" || r.decision() != "carry" {
			t.Fatalf("the sync run on a pure rebase of a green head did not carry: stopped at %s, continuity %v", r.stop, r.out["continuity"])
		}
		want := []string{"prepare", "continuity", "post_feedback", "feedback_health", "arm_automerge", "done"}
		if !slices.Equal(r.path, want) {
			t.Errorf("the carry walked %v, want %v", r.path, want)
		}
		for _, id := range r.path {
			if n := w.wf.Nodes[id]; n != nil && ir.NodeUsesLLM(n) {
				t.Errorf("the carry ran %s, a node that can call a model", id)
			}
		}
		// The static pin: no edge from the carry or standby decision reaches
		// a node that can call a model, run or not.
		for _, cond := range []string{"carry", "already_gated"} {
			for _, id := range gtReachable(t, w.wf, "continuity", cond) {
				if n := w.wf.Nodes[id]; n != nil && ir.NodeUsesLLM(n) {
					t.Errorf("the %s path can reach %s, a node that can call a model", cond, id)
				}
			}
		}
		// Carried from Y's carried green: the ORIGINAL audit travels.
		w.wantCarried(t, y2, x)
		w.wantArmedOn(t, y2)
		// CI the base requires passes on Y2: the forge merges the head the
		// verdict was vouched for, and nothing else.
		w.f.setCI(y2, "COMPLETED", "SUCCESS")
		if got := w.f.mergedHead(); got != y2 {
			t.Fatalf("merged %q, want the vouched head %s", got, y2[:12])
		}
		w.wantNoToken(t, true)
	})

	// 2. Mutants: delete the unanimity veto in the gate-history block (every
	// order carries or repins); drop the publish-time read in post_feedback
	// (the interleaved order paints a green over the hold).
	for _, order := range []string{"hold first", "green first", "interleaved"} {
		t.Run("2 a hold on any head of the change vetoes every later carry and repin/"+order, func(t *testing.T) {
			w := newGtWorld(t, gtGate, gtCI)
			x := w.bump(w.base0, "key20: v99")
			w.push(x)
			w.f.setCI(x, "IN_PROGRESS", "")
			w.launch(x, nil).audit(t, "clean")
			b1 := w.moveBase(w.base0, "two")
			y1 := w.bump(b1, "key20: v99")
			w.push(y1)
			w.f.setCI(y1, "IN_PROGRESS", "")
			b2 := w.moveBase(b1, "three")
			y2 := w.bump(b2, "key20: v99")
			switch order {
			case "hold first":
				// The /vetty audit of Y1 holds; the carry is asked for later.
				w.launch(y1, map[string]any{"continuity": false}).audit(t, "hold_unstable")
				w.push(y2)
				w.f.setCI(y2, "IN_PROGRESS", "")
			case "green first":
				// The sync run on Y1 carries first. The /vetty audit of Y1,
				// launched beside it, holds after a rebase moved the head:
				// its gate is refused, only its marker lands.
				hold := w.launch(y1, map[string]any{"continuity": false})
				g := w.launch(y1, nil)
				if g.decision() != "carry" {
					t.Fatalf("control: the sync run on Y1 did not carry: %v", g.out["continuity"])
				}
				w.wantCarried(t, y1, x)
				w.push(y2)
				w.f.setCI(y2, "IN_PROGRESS", "")
				hold.audit(t, "hold_unstable")
				if st := w.f.currentGate(y1); st == nil || st["state"] != "success" {
					t.Fatalf("control: the hold's gate was not refused for the moved head: %v", st)
				}
			case "interleaved":
				// The sync run on Y1 decides to carry; the /vetty hold of Y1
				// lands before that carry is published.
				g := w.launchUntil(y1, nil, "post_feedback")
				if g.decision() != "carry" {
					t.Fatalf("control: the sync run on Y1 did not decide to carry: %v", g.out["continuity"])
				}
				w.launch(y1, map[string]any{"continuity": false}).audit(t, "hold_unstable")
				g.resume(t)
				if st := w.f.currentGate(y1); st == nil || st["state"] != "failure" {
					t.Errorf("a carry decided before the hold was published over it: the gate on Y1 is %v", st)
				}
				if res := g.out["post_feedback"]; res["posted"] != false || !strings.Contains(fmt.Sprint(res["skipped_reason"]), "withdrawn") {
					t.Errorf("the carry was not withdrawn at publish time: %v", res)
				}
				if g.out["arm_automerge"]["armed"] != false {
					t.Errorf("a withdrawn carry was armed: %v", g.out["arm_automerge"])
				}
				w.push(y2)
				w.f.setCI(y2, "IN_PROGRESS", "")
			}
			// A later carry, on a later head of the same change: vetoed.
			r := w.launch(y2, nil)
			if r.decision() != "full" || !strings.Contains(fmt.Sprint(r.out["continuity"]["reason"]), "holds") {
				t.Errorf("a later carry was not vetoed by the hold: %v", r.out["continuity"])
			}
			w.wantNotGreen(t, y2)
			// A later repin: the full audit of Y2 finishes green after a
			// pure rebase moved the head to Y3. Its own verdict is its own;
			// moving it onto Y3 is a carry, and the hold vetoes it.
			b3 := w.moveBase(b2, "four")
			y3 := w.bump(b3, "key20: v99")
			w.push(y3)
			w.f.setCI(y3, "IN_PROGRESS", "")
			r.audit(t, "clean")
			if res := r.out["post_feedback"]; res["verdict"] != "clean" || !strings.Contains(fmt.Sprint(res["summary"]), "not repinned onto "+y3[:12]) {
				t.Errorf("a green was repinned onto %s over the hold: %v", y3[:12], res)
			}
			w.wantNotGreen(t, y3)
			if got := w.f.mergedHead(); got != "" {
				t.Errorf("merged %s past a hold on the change", got[:12])
			}
		})
	}

	// 3. Mutant: drop the edit veto in the gate-history block (every case
	// carries).
	for _, tamper := range []string{"origin_at rejuvenated", "the hold's marker deleted", "a marker appended"} {
		t.Run("3 an edited App review never yields a carry/"+tamper, func(t *testing.T) {
			w := newGtWorld(t, gtGate, gtCI)
			x := w.bump(w.base0, "key20: v99")
			w.push(x)
			w.f.setCI(x, "IN_PROGRESS", "")
			old := time.Now().UTC().Add(-100 * time.Hour)
			fresh := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05Z")
			b1 := w.moveBase(w.base0, "two")
			y1 := w.bump(b1, "key20: v99")
			switch tamper {
			case "origin_at rejuvenated":
				w.launchAt(x, nil, old).audit(t, "clean")
				w.f.editReview(1, "maintainer-alice", func(body string) string {
					return regexp.MustCompile(`origin_at=\S+`).ReplaceAllString(body, "origin_at="+fresh)
				})
			case "the hold's marker deleted":
				w.launch(x, nil).audit(t, "clean")
				w.push(y1)
				w.f.setCI(y1, "IN_PROGRESS", "")
				w.launch(y1, map[string]any{"continuity": false}).audit(t, "hold_unstable")
				w.f.editReview(2, "maintainer-alice", func(body string) string {
					return strings.TrimRight(body[:strings.LastIndex(body, "<!-- vetty-gate")], "\n")
				})
			case "a marker appended":
				w.launchAt(x, nil, old).audit(t, "clean")
				w.f.editReview(1, "maintainer-alice", func(body string) string {
					last := body[strings.LastIndex(body, "<!-- vetty-gate"):]
					return body + "\n" + regexp.MustCompile(`origin_at=\S+`).ReplaceAllString(last, "origin_at="+fresh)
				})
			}
			b2 := w.moveBase(b1, "three")
			y2 := w.bump(b2, "key20: v99")
			w.push(y2)
			w.f.setCI(y2, "IN_PROGRESS", "")
			r := w.launch(y2, nil)
			if r.decision() != "full" || !strings.Contains(fmt.Sprint(r.out["continuity"]["reason"]), "was edited") {
				t.Errorf("an edited App review did not veto the carry: %v", r.out["continuity"])
			}
			w.wantNotGreen(t, y2)
		})
	}

	// 4. Mutant: drop the creator check on a stamped status in the
	// gate-history block (both creators carry).
	for _, creator := range []map[string]any{
		{"login": "renovate[bot]", "id": 29139614, "type": "Bot"},
		{"login": "acme-pat-user", "id": 777, "type": "User"},
	} {
		t.Run("4 a status another identity wrote is never a source/"+creator["login"].(string), func(t *testing.T) {
			w := newGtWorld(t, gtGate, gtCI)
			x := w.bump(w.base0, "key20: v99")
			w.push(x)
			w.f.setCI(x, "IN_PROGRESS", "")
			w.launch(x, nil).audit(t, "clean")
			// Another identity re-posts the App's very status on X: the right
			// stamp, the right target_url — the forge now holds it current.
			own := w.f.currentGate(x)
			w.f.post(x, gtGate, "success", own["description"].(string), own["target_url"].(string), creator)
			b1 := w.moveBase(w.base0, "two")
			y := w.bump(b1, "key20: v99")
			w.push(y)
			w.f.setCI(y, "IN_PROGRESS", "")
			r := w.launch(y, nil)
			if r.decision() != "full" || !strings.Contains(fmt.Sprint(r.out["continuity"]["reason"]), "another identity") {
				t.Errorf("a status %s wrote was a source: %v", creator["login"], r.out["continuity"])
			}
			w.wantNotGreen(t, y)
		})
	}

	// 5. Mutants: drop the raw listing from the identity digest (the binary
	// cases carry); --unified=3 -> --unified=0 in the identity block (the
	// conflict-resolving rebase carries).
	t.Run("5 a change whose identity differs is never carried/a conflict-resolving rebase", func(t *testing.T) {
		w := newGtWorld(t, gtGate, gtCI)
		x := w.bump(w.base0, "key20: v99")
		w.push(x)
		w.f.setCI(x, "IN_PROGRESS", "")
		w.launch(x, nil).audit(t, "clean")
		// The base edits a context line of the hunk; the rebase resolves it.
		b1 := w.commit(w.base0, "base edits line 18", map[string][]byte{"deps.yaml": depsYAML(map[int]string{18: "key18: edited"})})
		w.setBase(b1)
		resolved := w.commit(b1, "bump, conflict resolved", map[string][]byte{"deps.yaml": depsYAML(map[int]string{18: "key18: edited", 20: "key20: v99"})})
		w.push(resolved)
		w.f.setCI(resolved, "IN_PROGRESS", "")
		w.wantFullAudit(t, w.launch(resolved, nil), "no green")
		w.wantNotGreen(t, resolved)
	})
	for _, pair := range []struct {
		name string
		a, b func(w *gtWorld) string
	}{
		{"a binary then a mode change", func(w *gtWorld) string {
			return w.commitModes(w.base0, "binary + chmod a", map[string][]byte{"logo.bin": gtBin('n')}, "scripts/a.sh")
		}, func(w *gtWorld) string {
			return w.commitModes(w.base0, "binary + chmod b", map[string][]byte{"logo.bin": gtBin('n')}, "scripts/b.sh")
		}},
		{"a binary then an empty file", func(w *gtWorld) string {
			return w.commit(w.base0, "binary + empty a", map[string][]byte{"logo.bin": gtBin('n'), "m/a.txt": {}})
		}, func(w *gtWorld) string {
			return w.commit(w.base0, "binary + empty b", map[string][]byte{"logo.bin": gtBin('n'), "m/b.txt": {}})
		}},
	} {
		t.Run("5 a change whose identity differs is never carried/"+pair.name, func(t *testing.T) {
			w := newGtWorld(t, gtGate, gtCI)
			a := pair.a(w)
			w.push(a)
			w.f.setCI(a, "IN_PROGRESS", "")
			w.launch(a, nil).audit(t, "clean")
			b := pair.b(w)
			w.push(b)
			w.f.setCI(b, "IN_PROGRESS", "")
			w.wantFullAudit(t, w.launch(b, nil), "no green")
			w.wantNotGreen(t, b)
		})
	}

	// 6. Mutants: drop the build check before a carry in continuity (the
	// carry decision of the gate-only base); drop it from post_feedback's
	// carry_vetoed (the repin carries onto the head and the standing arming
	// merges it); drop the carried door in arm_automerge (the standby rows
	// merge at the CLEAN and UNSTABLE doors, or arm on BEHIND).
	t.Run("6 a base that requires only the gate never merges nor arms a carried head", func(t *testing.T) {
		w := newGtWorld(t, gtGate)
		x := w.bump(w.base0, "key20: v99")
		w.push(x)
		w.f.setCI(x, "IN_PROGRESS", "")
		// The base also requires the branch up to date: X's arming is
		// accepted and stands, as GitHub keeps it across a rebase. Two audits
		// of X run side by side (a synchronize and a /vetty, say); the second
		// is still in flight when the pull request is rebased.
		w.f.setBehind(true)
		r0 := w.launch(x, nil)
		r1 := w.launch(x, nil)
		r1.wantStop(t, "security_audit")
		r0.audit(t, "clean")
		if !w.f.armed(x) {
			t.Fatalf("control: X's green was not armed on the BEHIND pull request: %v", r0.out["arm_automerge"])
		}
		b1 := w.moveBase(w.base0, "two")
		y := w.bump(b1, "key20: v99")
		w.push(y)
		w.f.setBehind(false)
		w.f.setCI(y, "IN_PROGRESS", "")
		// It finishes green after the rebase: a repin would paint Y green,
		// and the standing arming would merge it on the gate alone.
		r1.audit(t, "clean")
		if res := r1.out["post_feedback"]; res["verdict"] != "clean" || !strings.Contains(fmt.Sprint(res["summary"]), "attests no build") {
			t.Errorf("a green was repinned onto a head nothing would build: %v", res)
		}
		w.wantNotGreen(t, y)
		// The sync run on Y: nothing the base requires would build it, so
		// the full audit builds it — no carry.
		w.wantFullAudit(t, w.launch(y, nil), "attests no build")
		w.wantNotGreen(t, y)
		if got := w.f.mergedHead(); got != "" {
			t.Fatalf("merged %s on a gate-only base", got[:12])
		}

		// The doors. Y carries a carried green from a time the base required
		// CI (and arming was off); the operator then drops CI from the
		// required checks and turns arming on. A re-delivery on Y renews the
		// arming through the standby path, with the carried verdict.
		w2 := newGtWorld(t, gtGate, gtCI)
		x2 := w2.bump(w2.base0, "key20: v99")
		w2.push(x2)
		w2.f.setCI(x2, "IN_PROGRESS", "")
		off := map[string]any{"arm_automerge": false}
		w2.launch(x2, off).audit(t, "clean")
		b := w2.moveBase(w2.base0, "two")
		y2 := w2.bump(b, "key20: v99")
		w2.push(y2)
		w2.f.setCI(y2, "IN_PROGRESS", "")
		if r := w2.launch(y2, off); r.decision() != "carry" {
			t.Fatalf("control: Y was not carried while the base required CI: %v", r.out["continuity"])
		}
		w2.f.setRequired(gtGate)
		for _, row := range []struct {
			name           string
			status, concl  string
			behind, absent bool
		}{
			{"UNSTABLE, CI red and not required", "COMPLETED", "FAILURE", false, false},
			{"UNSTABLE, CI still running and not required", "IN_PROGRESS", "", false, false},
			{"CLEAN, CI green and not required", "COMPLETED", "SUCCESS", false, false},
			{"CLEAN, no CI context at all", "", "", false, true},
			{"BEHIND: GitHub would accept the arming", "COMPLETED", "FAILURE", true, false},
		} {
			if row.absent {
				w2.f.dropCI(y2)
			} else {
				w2.f.setCI(y2, row.status, row.concl)
			}
			w2.f.setBehind(row.behind)
			r := w2.launch(y2, nil)
			if r.decision() != "already_gated" || r.out["continuity_standby"]["verdict"] != "carried" {
				t.Fatalf("%s: control: the re-delivery did not renew Y's carried verdict: %v", row.name, r.out["continuity"])
			}
			if am := r.out["arm_automerge"]; am["armed"] != false || !strings.Contains(fmt.Sprint(am["reason"]), "attests no build") {
				t.Errorf("%s: a carried head nothing builds was not refused: %v", row.name, am)
			}
			if w2.f.armed(y2) || w2.f.mergedHead() != "" {
				t.Fatalf("%s: a carried head nothing builds was armed (%v) or merged (%q)", row.name, w2.f.armed(y2), w2.f.mergedHead())
			}
		}
		// The control: CI the base requires, green, is the build — the
		// carried head merges, pinned to the vouched head.
		w2.f.setBehind(false)
		w2.f.setRequired(gtGate, gtCI)
		w2.f.setCI(y2, "COMPLETED", "SUCCESS")
		r := w2.launch(y2, nil)
		if am := r.out["arm_automerge"]; am["armed"] != true {
			t.Fatalf("a carried head CI the base requires builds was not merged: %v", am)
		}
		if got := w2.f.mergedHead(); got != y2 || w2.f.mergedHow() != "mergePullRequest@"+y2 {
			t.Fatalf("merged %q (%s), want a merge pinned to the vouched head %s", got, w2.f.mergedHow(), y2[:12])
		}
	})

	// 7. Mutants: write `now` as the origin_at of a carried verdict in
	// post_feedback (the chain extends the clock: the last head carries,
	// and the resumed run publishes); drop the age comparison in continuity
	// (the last head carries); drop the age term of post_feedback's
	// withdrawal (the resumed run publishes).
	t.Run("7 past carry_max_age_h from the original audit nothing is carried, however chained", func(t *testing.T) {
		w := newGtWorld(t, gtGate, gtCI)
		now := time.Now().UTC().Truncate(time.Second)
		t0 := now.Add(-80 * time.Hour)
		x := w.bump(w.base0, "key20: v99")
		w.push(x)
		w.f.setCI(x, "IN_PROGRESS", "")
		w.launchAt(x, nil, t0).audit(t, "clean")
		heads := []string{x}
		base := w.base0
		for i, at := range []time.Duration{30 * time.Hour, 60 * time.Hour} {
			base = w.moveBase(base, "chain "+strconv.Itoa(i))
			y := w.bump(base, "key20: v99")
			w.push(y)
			w.f.setCI(y, "IN_PROGRESS", "")
			if r := w.launchAt(y, nil, t0.Add(at)); r.decision() != "carry" {
				t.Fatalf("control: hop %d was not carried: %v", i+1, r.out["continuity"])
			}
			if m := w.f.markerOn(y); m["origin"] != x || m["origin_at"] != t0.Format("2006-01-02T15:04:05Z") {
				t.Errorf("hop %d restarted the clock: its marker names %v, want origin %s at %s", i+1, m, x[:12], t0)
			}
			heads = append(heads, y)
		}
		// A run that decided its carry 71h after the original audit and
		// publishes it 73h after (resumed after a pause): withdrawn.
		base = w.moveBase(base, "chain 2")
		y3 := w.bump(base, "key20: v99")
		w.push(y3)
		w.f.setCI(y3, "IN_PROGRESS", "")
		late := w.launchUntilAt(y3, nil, "post_feedback", t0.Add(71*time.Hour))
		if late.decision() != "carry" {
			t.Fatalf("control: within the bound, the third hop was not decided a carry: %v", late.out["continuity"])
		}
		late.clock = t0.Add(73 * time.Hour)
		late.resume(t)
		if res := late.out["post_feedback"]; res["posted"] != false || !strings.Contains(fmt.Sprint(res["skipped_reason"]), "older than carry_max_age_h") {
			t.Errorf("a carry published past carry_max_age_h from the original audit: %v", res)
		}
		w.wantNotGreen(t, y3)
		// And at 80h, no carry is decided at all.
		w.wantFullAudit(t, w.launchAt(y3, nil, now), "older than carry_max_age_h")
		w.wantNotGreen(t, y3)
	})

	// 8 is asserted at the end of every world above (wantNoToken, from its
	// cleanup); this row adds the positive control on the channel the token
	// DOES travel: the git endpoint received it as a header, and the API as
	// a bearer. Mutant: pass the repin fetch's header as `-c
	// http.extraHeader=…` on git's command line (the argv assertion reddens).
	t.Run("8 the forge token never reaches an argv nor any recorded output", func(t *testing.T) {
		w := newGtWorld(t, gtGate, gtCI)
		x := w.bump(w.base0, "key20: v99")
		w.push(x)
		w.f.setCI(x, "IN_PROGRESS", "")
		r0 := w.launch(x, nil)
		b1 := w.moveBase(w.base0, "two")
		y := w.bump(b1, "key20: v99")
		w.push(y)
		w.f.setCI(y, "IN_PROGRESS", "")
		r0.audit(t, "clean")
		w.wantCarried(t, y, x)
		w.wantNoToken(t, true)
	})
}

// ─── the world: one pull request, its forge, its git ─────────────────────

const (
	gtToken    = "ghs_guaranteeSecretToken0123456789"
	gtPubToken = "itr_guaranteePublishGrant0123456789"
	gtGate     = "vetty/deps"
	gtCI       = "ci/test"
	gtBranch   = "renovate/bump"
	// gtAppLogin is the App as GraphQL spells it; REST says vettyLogin.
	gtAppLogin     = "acme-vetty"
	gtTimeLayout   = "2006-01-02T15:04:05Z"
	gtInFlightNote = "review in progress — the verdict will replace this"
)

func gtBin(b byte) []byte { return []byte{0, 1, 2, 3, 0, b} }

type gtWorld struct {
	t         *testing.T
	f         *gtForge
	api, git  *httptest.Server
	seed      string
	origin    forgeOrigin
	tokenPath string
	argvLog   string
	env       []string
	wf        *ir.Workflow
	vars      map[string]any
	base0     string
	mu        sync.Mutex
	stdout    []string
	checked   bool
}

func newGtWorld(t *testing.T, required ...string) *gtWorld {
	t.Helper()
	w := &gtWorld{t: t, wf: compilePlanPhaseBot(t, "dep-update-guard")}
	w.f = &gtForge{t: t, required: map[string]bool{}, ci: map[string]map[string]any{},
		statuses: map[string][]map[string]any{}}
	w.f.setRequired(required...)
	w.api = httptest.NewServer(w.f)
	t.Cleanup(w.api.Close)

	w.origin = newForgeOrigin(t)
	gittest.Run(t, w.origin.dir, "symbolic-ref", "HEAD", "refs/heads/main")
	backend := filepath.Join(gittest.Run(t, w.origin.dir, "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git-http-backend is not where git says its helpers are: %v", err)
	}
	cgiHandler := &cgi.Handler{Path: backend, Env: []string{
		"GIT_PROJECT_ROOT=" + w.origin.root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
	}}
	w.git = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.f.mu.Lock()
		w.f.gitAuth = append(w.f.gitAuth, r.Header.Get("Authorization"))
		w.f.mu.Unlock()
		cgiHandler.ServeHTTP(rw, r)
	}))
	t.Cleanup(w.git.Close)

	w.tokenPath = filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(w.tokenPath, []byte(gtToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	w.argvLog = filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + w.argvLog + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kv := range gittest.Env() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(name, "_PROXY") || name == "PATH" || strings.HasPrefix(name, "GIT_CONFIG_") {
			continue
		}
		w.env = append(w.env, kv)
	}
	w.env = append(w.env,
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_SSL_NO_VERIFY=1",
		// What the sandbox carries in git's environment, and what keeps a
		// test's git from detaching auto-maintenance under t.TempDir().
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
		secretguard.PlaceholderForName(store.ForgePublishTokenVar)+"="+gtPubToken)

	w.seed = t.TempDir()
	gittest.InitRepo(t, w.seed)
	w.base0 = w.commit("HEAD", "base", map[string][]byte{
		"deps.yaml": depsYAML(nil), "other.txt": []byte("one\n"),
		"logo.bin": gtBin('o'), "scripts/a.sh": []byte("#!/bin/sh\n"), "scripts/b.sh": []byte("#!/bin/sh\n"),
	})
	w.setBase(w.base0)

	w.vars = map[string]any{
		"base_ref": "main", "pr_url": "https://github.com/acme/widgets/pull/7", "pr_author": "renovate[bot]",
		"scope_notes": "", "max_fix_iterations": 2, "post_to_board": false, "gate_enabled": true,
		"gate_context": gtGate, "arm_automerge": true, "respect_human_arming": true, "automerge_method": "squash",
		"automerge_on_unstable": "merge", "automerge_settle_s": 1, "continuity": true, "carry_max_age_h": 72,
		"forge_publish_url": w.api.URL + "/api/v1/forge/publish-review", "forge_publish_token": "",
		"forge_delivery_preflight_url": "", "verify_timeout_s": 1200,
	}
	// Assertion 8 holds in every world, whatever row drove it.
	t.Cleanup(func() {
		if !w.checked && !t.Failed() {
			w.wantNoToken(t, false)
		}
	})
	return w
}

func (w *gtWorld) commit(parent, msg string, files map[string][]byte) string {
	w.t.Helper()
	return commitTree(w.t, w.seed, parent, msg, files)
}

func (w *gtWorld) commitModes(parent, msg string, files map[string][]byte, executable ...string) string {
	w.t.Helper()
	return commitModes(w.t, w.seed, parent, msg, files, executable...)
}

// bump is the dependency bump: deps.yaml's line 20 set to `line`, on parent.
func (w *gtWorld) bump(parent, line string) string {
	w.t.Helper()
	return w.commit(parent, "bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: line})})
}

// moveBase commits an unrelated change on parent and makes it the base.
func (w *gtWorld) moveBase(parent, other string) string {
	w.t.Helper()
	b := w.commit(parent, "base moves", map[string][]byte{"other.txt": []byte(other + "\n")})
	w.setBase(b)
	return b
}

func (w *gtWorld) setBase(sha string) {
	w.t.Helper()
	gittest.Run(w.t, w.seed, "push", "-q", "--force", w.origin.dir, sha+":refs/heads/main")
}

// push force-pushes the pull request's branch to sha: its new head.
func (w *gtWorld) push(sha string) {
	w.t.Helper()
	gittest.Run(w.t, w.seed, "push", "-q", "--force", w.origin.dir, sha+":refs/heads/"+gtBranch)
	w.f.push(sha)
}

// ─── a run ───────────────────────────────────────────────────────────────

type gtRun struct {
	w     *gtWorld
	ws    string
	vars  map[string]any
	out   map[string]map[string]any
	stubs map[string]map[string]any
	path  []string
	stop  string
	// clock, when set, is the time every script of the run reads.
	clock time.Time
	// next / nextIn: where a paused run resumes.
	next   string
	nextIn map[string]any
	pause  string
}

// launch is a synchronize (or, with continuity off, a /vetty) launch on
// head: the server claims the gate, the runner clones the pull request, and
// the graph runs until it ends or reaches a model node.
func (w *gtWorld) launch(head string, over map[string]any) *gtRun {
	w.t.Helper()
	return w.launchUntilAt(head, over, "", time.Time{})
}

func (w *gtWorld) launchAt(head string, over map[string]any, at time.Time) *gtRun {
	w.t.Helper()
	return w.launchUntilAt(head, over, "", at)
}

func (w *gtWorld) launchUntil(head string, over map[string]any, pause string) *gtRun {
	w.t.Helper()
	return w.launchUntilAt(head, over, pause, time.Time{})
}

func (w *gtWorld) launchUntilAt(head string, over map[string]any, pause string, at time.Time) *gtRun {
	t := w.t
	t.Helper()
	w.f.claim(head)
	ws := filepath.Join(t.TempDir(), "ws")
	gittest.Run(t, filepath.Dir(ws), "clone", "-q", "--no-tags", w.origin.dir, ws)
	gittest.Run(t, ws, "config", "maintenance.auto", "false")
	gittest.Run(t, ws, "config", "gc.auto", "0")
	gittest.Run(t, ws, "checkout", "-q", "-B", gtBranch, head)
	r := &gtRun{w: w, ws: ws, vars: map[string]any{}, out: map[string]map[string]any{},
		stubs: map[string]map[string]any{}, clock: at, pause: pause}
	for k, v := range w.vars {
		r.vars[k] = v
	}
	for k, v := range over {
		r.vars[k] = v
	}
	r.vars["workspace_dir"] = ws
	r.vars["scratch_dir"] = filepath.Join(t.TempDir(), "scratch")
	r.drive("prepare", nil)
	return r
}

// decision is continuity's answer: carry, already_gated, or full.
func (r *gtRun) decision() string {
	c := r.out["continuity"]
	switch {
	case c == nil:
		return ""
	case c["carry"] == true:
		return "carry"
	case c["already_gated"] == true:
		return "already_gated"
	}
	return "full"
}

func (r *gtRun) wantStop(t *testing.T, node string) {
	t.Helper()
	if r.stop != node {
		t.Fatalf("the run stopped at %q (path %v), want %q", r.stop, r.path, node)
	}
}

// audit finishes a run that stopped at the full audit: the model nodes, and
// the probes reading what they did, answer as an audit ending in verdict
// would; every gate, the publish and the arming run for real.
func (r *gtRun) audit(t *testing.T, verdict string) {
	t.Helper()
	r.wantStop(t, "security_audit")
	safe := verdict != "hold_security"
	r.stubs = map[string]map[string]any{
		"security_audit": {"verdict": map[bool]string{true: "safe", false: "suspicious"}[safe], "cves": []any{},
			"malware_signals": []any{}, "blockers": []any{}, "auditors_consulted": []any{"osv"}, "summary": "audited"},
		"align": {"applied": false, "files_changed": []any{}, "breaking_summary": "", "needs_human": false,
			"human_question": "", "summary": "nothing to align"},
		"align_probe":  {"pending": false, "carried": false, "paths": []any{}, "unaccounted": []any{}, "reason": "clean tree"},
		"verify_build": {"prepared": true, "summary": "verify.sh written"},
		"verify_run": {"passed": verdict != "hold_unstable", "skipped": false, "exit_code": map[bool]int{true: 0, false: 1}[verdict != "hold_unstable"],
			"log_tail": "ok", "precheck_rejected": false, "precheck_reason": ""},
		"delivery_probe": {"ok": true, "denied": false, "code": "", "reason": "nothing to deliver"},
		"commit":         {"committed": false, "pushed": false, "sha": "", "message": "", "summary": "nothing to commit"},
		"commit_probe":   {"sha": "", "moved": false, "reason": "the workspace head did not move"},
	}
	r.drive(r.next, r.nextIn)
	if r.stop != "done" {
		t.Fatalf("the audit ended at %s (path %v)", r.stop, r.path)
	}
	if got := r.out["post_feedback"]["verdict"]; verdict != "clean" && got != verdict {
		t.Fatalf("the audit published %v, want %s", got, verdict)
	}
}

func (r *gtRun) resume(t *testing.T) {
	t.Helper()
	r.pause = ""
	r.drive(r.next, r.nextIn)
}

// drive runs the graph from node, its input in, until it ends, reaches a
// model node with no stub (the full audit), or reaches the pause node.
func (r *gtRun) drive(node string, in map[string]any) {
	t := r.w.t
	t.Helper()
	for steps := 0; ; steps++ {
		if steps > 64 {
			t.Fatalf("the drive did not end: %v", r.path)
		}
		r.next, r.nextIn = node, in
		if node == r.pause && r.pause != "" {
			r.stop = "paused before " + node
			return
		}
		n := r.w.wf.Nodes[node]
		switch {
		case n == nil && (node == "done" || node == "fail"):
			r.path, r.stop = append(r.path, node), node
			return
		case n == nil:
			t.Fatalf("the graph reached %q, which is no node", node)
		}
		var out map[string]any
		if stub, ok := r.stubs[node]; ok {
			out = stub
		} else {
			switch n := n.(type) {
			case *ir.DoneNode, *ir.FailNode:
				r.path, r.stop = append(r.path, node), node
				return
			case *ir.ToolNode:
				out = r.tool(node, n, in)
			case *ir.ComputeNode:
				out = r.compute(n, in)
			default:
				// A model node with nothing standing in for it: the run
				// takes the full audit from here.
				r.stop = node
				return
			}
		}
		r.out[node] = out
		r.path = append(r.path, node)
		e := r.edge(node)
		in = r.with(e)
		node = e.To
	}
}

// edge is the edge the engine takes out of node: the first conditional one
// that holds, in declaration order, else the `else` one, else the
// unconditional one.
func (r *gtRun) edge(node string) *ir.Edge {
	t := r.w.t
	t.Helper()
	var fallback *ir.Edge
	for _, e := range r.w.wf.Edges {
		if e.From != node {
			continue
		}
		switch {
		case e.Expression != nil:
			ok, err := e.Expression.EvalBool(r.ctx(r.out[node]))
			if err != nil {
				t.Fatalf("edge %s -> %s: %v", e.From, e.To, err)
			}
			if ok {
				return e
			}
		case e.Condition != "":
			if gtTruthy(r.out[node][e.Condition]) != e.Negated {
				return e
			}
		default:
			fallback = e
		}
	}
	if fallback == nil {
		t.Fatalf("no edge out of %s holds for %v", node, r.out[node])
	}
	return fallback
}

func gtTruthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	case nil:
		return false
	}
	return true
}

func (r *gtRun) ctx(input map[string]any) *expr.Context {
	get := func(m map[string]any, path []string) any {
		var v any = m
		for _, p := range path {
			mm, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = mm[p]
		}
		return v
	}
	return &expr.Context{
		Vars:  func(p []string) any { return get(r.vars, p) },
		Input: func(p []string) any { return get(input, p) },
		Outputs: func(p []string) any {
			if len(p) == 0 {
				return nil
			}
			return get(r.out[p[0]], p[1:])
		},
	}
}

var gtRef = regexp.MustCompile(`\{\{\s*([a-z_]+)\.([A-Za-z0-9_.]+)\s*\}\}`)

// lookup resolves one reference of an edge mapping.
func (r *gtRun) lookup(ns, path string, src map[string]any) any {
	parts := strings.Split(path, ".")
	switch ns {
	case "outputs":
		return r.ctx(nil).Outputs(parts)
	case "vars":
		return r.ctx(nil).Vars(parts)
	case "input":
		return r.ctx(src).Input(parts)
	}
	r.w.t.Fatalf("an edge maps {{%s.%s}}, a namespace this drive does not resolve", ns, path)
	return nil
}

// with is the input the edge hands its target: a mapping that is one
// reference takes the value as is; any other is the template with every
// reference rendered as text.
func (r *gtRun) with(e *ir.Edge) map[string]any {
	in := map[string]any{}
	src := r.out[e.From]
	for _, dm := range e.With {
		raw := strings.TrimSpace(dm.Raw)
		if m := gtRef.FindStringSubmatch(raw); m != nil && m[0] == raw {
			in[dm.Key] = r.lookup(m[1], m[2], src)
			continue
		}
		in[dm.Key] = gtRef.ReplaceAllStringFunc(dm.Raw, func(s string) string {
			m := gtRef.FindStringSubmatch(s)
			v := r.lookup(m[1], m[2], src)
			switch v := v.(type) {
			case nil:
				return ""
			case string:
				return v
			}
			b, _ := json.Marshal(v)
			return string(b)
		})
	}
	return in
}

func (r *gtRun) compute(n *ir.ComputeNode, in map[string]any) map[string]any {
	out := map[string]any{}
	for _, ce := range n.Exprs {
		v, err := ce.AST.Eval(r.ctx(in))
		if err != nil {
			r.w.t.Fatalf("compute %s.%s: %v", n.ID, ce.Key, err)
		}
		out[ce.Key] = v
	}
	return out
}

// tool runs a tool node the way the executor does: a script with every
// reference a JSON literal (null when it resolves to nothing), a command
// with every reference shell-escaped — the forge's addresses pointed at the
// world's.
func (r *gtRun) tool(id string, n *ir.ToolNode, in map[string]any) map[string]any {
	w := r.w
	t := w.t
	t.Helper()
	script := strings.TrimSpace(n.Script) != ""
	body := n.Command
	if script {
		body = n.Script
		body = strings.ReplaceAll(body, `"https://api.github.com/graphql"`, quoted(w.api.URL+"/graphql"))
		body = strings.ReplaceAll(body, `"https://api.github.com"`, quoted(w.api.URL))
		body = strings.ReplaceAll(body, `"https://github.com/"`, quoted(w.git.URL+"/"))
	}
	render := func(v any) string {
		if script {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		s := ""
		switch v := v.(type) {
		case nil:
		case string:
			s = v
		default:
			b, _ := json.Marshal(v)
			s = string(b)
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	body = gtRef.ReplaceAllStringFunc(body, func(s string) string {
		m := gtRef.FindStringSubmatch(s)
		switch m[1] {
		case "input":
			return render(in[m[2]])
		case "vars":
			v, ok := r.vars[m[2]]
			if !ok {
				t.Fatalf("%s reads {{vars.%s}}, which this world does not set", id, m[2])
			}
			return render(v)
		case "secrets":
			if m[2] != "forge_token.path" {
				t.Fatalf("%s reads {{secrets.%s}}", id, m[2])
			}
			return render(w.tokenPath)
		case "run":
			return render(map[string]string{"url": "https://iterion.example/runs/01gt", "id": "01gt"}[m[2]])
		}
		t.Fatalf("%s reads %s, a namespace this drive does not render", id, s)
		return ""
	})
	var cmd *exec.Cmd
	if script {
		if !r.clock.IsZero() {
			// The run's clock: every time.time() and time.gmtime() the script
			// reads is this instant.
			at := strconv.FormatInt(r.clock.Unix(), 10)
			body = "import time as _gt_time\n_gt_gm = _gt_time.gmtime\n" +
				"_gt_time.time = lambda: float(" + at + ")\n" +
				"_gt_time.gmtime = lambda s=None: _gt_gm(" + at + " if s is None else s)\n" + body
		}
		path := filepath.Join(t.TempDir(), id+".py")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command("python3", "-I", path)
	} else {
		cmd = exec.Command("bash", "-c", body)
	}
	cmd.Dir = t.TempDir()
	cmd.Env = w.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	w.mu.Lock()
	w.stdout = append(w.stdout, id+": "+string(raw)+stderr.String())
	w.mu.Unlock()
	if err != nil {
		t.Fatalf("%s failed: %v\nstdout: %s\nstderr: %s", id, err, raw, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s did not answer JSON: %v (%q)", id, err, raw)
	}
	return out
}

// gtReachable lists the nodes reachable from the target of `from`'s
// `when cond` edge.
func gtReachable(t *testing.T, wf *ir.Workflow, from, cond string) []string {
	t.Helper()
	var queue []string
	for _, e := range wf.Edges {
		if e.From == from && e.Condition == cond && !e.Negated {
			queue = append(queue, e.To)
		}
	}
	if len(queue) != 1 {
		t.Fatalf("%s has %d `when %s` edges, want one", from, len(queue), cond)
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, e := range wf.Edges {
			if e.From == id {
				queue = append(queue, e.To)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ─── what the world asserts ──────────────────────────────────────────────

// wantCarried: the gate on head is this App's green, stamped with the
// change, linking the review whose last-line marker says `carried` from the
// original audit `origin`.
func (w *gtWorld) wantCarried(t *testing.T, head, origin string) {
	t.Helper()
	st := w.f.currentGate(head)
	if st == nil || st["state"] != "success" {
		t.Fatalf("no green gate on %s: %v", head[:12], st)
	}
	m := w.f.markerOn(head)
	if m == nil || m["verdict"] != "carried" || m["origin"] != origin {
		t.Fatalf("the gate on %s is not a carried verdict from %s: marker %v", head[:12], origin[:12], m)
	}
	if !strings.HasSuffix(st["description"].(string), " · "+m["id"]) || st["target_url"] != m["url"] {
		t.Errorf("the carried status on %s is not stamped with %s and linked to its review: %v", head[:12], m["id"], st)
	}
}

func (w *gtWorld) wantNotGreen(t *testing.T, head string) {
	t.Helper()
	if st := w.f.currentGate(head); st != nil && st["state"] == "success" {
		t.Errorf("%s carries a green gate: %v (marker %v)", head[:12], st, w.f.markerOn(head))
	}
	if m := w.f.markerOn(head); m != nil && m["verdict"] == "carried" {
		t.Errorf("a carried verdict was published about %s: %v", head[:12], m)
	}
}

func (w *gtWorld) wantArmedOn(t *testing.T, head string) {
	t.Helper()
	if !w.f.armed(head) {
		t.Errorf("no arming was placed pinned to %s", head[:12])
	}
}

func (w *gtWorld) wantFullAudit(t *testing.T, r *gtRun, reason string) {
	t.Helper()
	if r.decision() != "full" || r.stop != "security_audit" {
		t.Errorf("want the full audit, got %s stopping at %s: %v", r.decision(), r.stop, r.out["continuity"])
	}
	if got := fmt.Sprint(r.out["continuity"]["reason"]); !strings.Contains(got, reason) {
		t.Errorf("the full audit's reason %q does not say %q", got, reason)
	}
}

// wantNoToken: the forge token is in no git argv, in no script's output, in
// no request the forge recorded (its credential headers aside) and in
// nothing the forge holds. channel adds the positive control: the git
// endpoint DID receive it as a header, and every API read carried it as a
// bearer (the forge checks that on each request).
func (w *gtWorld) wantNoToken(t *testing.T, channel bool) {
	t.Helper()
	w.checked = true
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + gtToken))
	leak := func(where, s string) {
		for _, secret := range []string{gtToken, encoded, gtPubToken} {
			if strings.Contains(s, secret) {
				t.Errorf("a credential reached %s:\n%s", where, s)
			}
		}
	}
	if argv, err := os.ReadFile(w.argvLog); err == nil {
		leak("git's argv", string(argv))
	} else if channel {
		t.Errorf("the git shim recorded nothing: %v", err)
	}
	w.mu.Lock()
	for _, s := range w.stdout {
		leak("a script's output", s)
	}
	w.mu.Unlock()
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for _, s := range w.f.recorded {
		leak("a request the forge recorded", s)
	}
	held, _ := json.Marshal(map[string]any{"reviews": w.f.reviews, "statuses": w.f.statuses})
	leak("what the forge holds", string(held))
	if channel && !slices.Contains(w.f.gitAuth, "Basic "+encoded) {
		t.Errorf("the git endpoint never received the token as a header (saw %q): the argv check proves nothing", w.f.gitAuth)
	}
}

// ─── the forge ───────────────────────────────────────────────────────────

// gtForge is GitHub for acme/widgets#7 plus the iterion server's publish
// endpoint, with state.
type gtForge struct {
	t        *testing.T
	mu       sync.Mutex
	head     string
	required map[string]bool
	ci       map[string]map[string]any // sha → the CI check run on it
	behind   bool
	reviews  []map[string]any
	statuses map[string][]map[string]any // sha → in the order posted
	nextID   int
	// autoMerge is the standing arming: who placed it.
	autoMerge string
	armedOn   []string
	merged    string
	how       string
	recorded  []string
	gitAuth   []string
}

func (f *gtForge) setRequired(contexts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.required = map[string]bool{}
	for _, c := range contexts {
		f.required[c] = true
	}
	f.settle()
}

func (f *gtForge) setBehind(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behind = b
	f.settle()
}

func (f *gtForge) setCI(sha, status, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var c any
	if conclusion != "" {
		c = conclusion
	}
	f.ci[sha] = map[string]any{"__typename": "CheckRun", "name": gtCI, "status": status, "conclusion": c}
	f.settle()
}

func (f *gtForge) dropCI(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ci, sha)
	f.settle()
}

// push moves the head. An arming stands across it, as GitHub keeps one
// when the pusher has write access.
func (f *gtForge) push(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = sha
	f.settle()
}

func (f *gtForge) mergedHead() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merged
}

func (f *gtForge) mergedHow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.how
}

func (f *gtForge) armed(sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.armedOn, sha)
}

// post appends a status; the last one posted per context is the current one.
func (f *gtForge) post(sha, context, state, description, target string, creator map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postLocked(sha, context, state, description, target, creator)
}

func (f *gtForge) postLocked(sha, context, state, description, target string, creator map[string]any) {
	f.nextID++
	f.statuses[sha] = append(f.statuses[sha], map[string]any{
		"id": 5_000_000 + f.nextID, "context": context, "state": state, "description": description,
		"target_url": target, "creator": creator,
	})
	f.settle()
}

func gtApp() map[string]any { return map[string]any{"login": vettyLogin, "id": vettyID, "type": "Bot"} }

// claim is the server's launch claim: a pending in-flight marker, only ever
// over nothing or over another in-flight marker — never over a verdict.
func (f *gtForge) claim(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.current(sha, gtGate); st == nil || (st["state"] == "pending" && st["description"] == gtInFlightNote) {
		f.postLocked(sha, gtGate, "pending", gtInFlightNote, "https://iterion.example/runs/claim", gtApp())
	}
}

// editReview rewrites review n's body as someone with write access does:
// its author stays the App, and the forge records the edit.
func (f *gtForge) editReview(n int, editor string, edit func(string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reviews[n-1]
	r["body"] = edit(r["body"].(string))
	r["lastEditedAt"] = time.Now().UTC().Format(gtTimeLayout)
	r["editor"] = map[string]any{"login": editor}
}

func (f *gtForge) current(sha, context string) map[string]any {
	var cur map[string]any
	for _, st := range f.statuses[sha] {
		if st["context"] == context {
			cur = st
		}
	}
	return cur
}

func (f *gtForge) currentGate(sha string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current(sha, gtGate)
}

// markerOn is the last-line marker of the review the current gate on sha
// links, with that review's url.
func (f *gtForge) markerOn(sha string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.current(sha, gtGate)
	if st == nil {
		return nil
	}
	for _, r := range f.reviews {
		if r["html_url"] != st["target_url"] {
			continue
		}
		body := strings.TrimRight(r["body"].(string), "\n")
		m := markerLine.FindStringSubmatch(body[strings.LastIndex(body, "\n")+1:])
		if m == nil {
			return nil
		}
		return map[string]string{"sha": m[1], "id": m[2], "verdict": m[3], "origin": m[4], "origin_at": m[5], "url": r["html_url"].(string)}
	}
	return nil
}

// rollup is the check rollup on sha: every current status and the CI check
// run, each with whether the base requires it.
func (f *gtForge) rollup(sha string) []map[string]any {
	var nodes []map[string]any
	seen := map[string]bool{}
	for i := len(f.statuses[sha]) - 1; i >= 0; i-- {
		st := f.statuses[sha][i]
		ctx := st["context"].(string)
		if seen[ctx] {
			continue
		}
		seen[ctx] = true
		nodes = append(nodes, map[string]any{"__typename": "StatusContext", "context": ctx,
			"state": strings.ToUpper(st["state"].(string)), "isRequired": f.required[ctx]})
	}
	if c := f.ci[sha]; c != nil {
		n := map[string]any{"isRequired": f.required[gtCI]}
		for k, v := range c {
			n[k] = v
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return fmt.Sprint(nodes[i]) < fmt.Sprint(nodes[j]) })
	return nodes
}

func gtPassing(n map[string]any) bool {
	if n["__typename"] == "StatusContext" {
		return n["state"] == "SUCCESS"
	}
	return n["status"] == "COMPLETED" && slices.Contains([]any{"SUCCESS", "NEUTRAL", "SKIPPED"}, n["conclusion"])
}

// state is mergeStateStatus: a required context not passing (or absent)
// blocks; then BEHIND; then any other context not passing is UNSTABLE.
func (f *gtForge) state() string {
	nodes := f.rollup(f.head)
	names := map[string]map[string]any{}
	for _, n := range nodes {
		name, _ := n["context"].(string)
		if name == "" {
			name, _ = n["name"].(string)
		}
		names[name] = n
	}
	for c := range f.required {
		if n := names[c]; n == nil || !gtPassing(n) {
			return "BLOCKED"
		}
	}
	if f.behind {
		return "BEHIND"
	}
	for _, n := range nodes {
		if !gtPassing(n) {
			return "UNSTABLE"
		}
	}
	return "CLEAN"
}

// settle is the forge merging an armed pull request once nothing it waits
// for is left: every required check passing, the branch up to date.
func (f *gtForge) settle() {
	if f.autoMerge == "" || f.merged != "" || f.head == "" {
		return
	}
	if s := f.state(); s == "CLEAN" || s == "UNSTABLE" {
		f.merged, f.how = f.head, "auto-merge armed by "+f.autoMerge
	}
}

func (f *gtForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var hdr []string
	for k, vs := range r.Header {
		if k != "Authorization" && k != "X-Iterion-Run" {
			hdr = append(hdr, k+": "+strings.Join(vs, ","))
		}
	}
	sort.Strings(hdr)
	f.recorded = append(f.recorded, r.Method+" "+r.URL.String()+"\n"+strings.Join(hdr, "\n")+"\n"+string(raw))
	if r.URL.Path == "/api/v1/forge/publish-review" {
		if got := r.Header.Get("X-Iterion-Run"); got != gtPubToken {
			f.t.Errorf("the publish was not authenticated with the run's grant (%q)", got)
		}
		f.publish(w, raw)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+gtToken {
		f.t.Errorf("%s %s was not made with the run token (%q)", r.Method, r.URL.Path, got)
	}
	if r.URL.Path == "/graphql" && r.Method == http.MethodPost {
		f.graphql(w, r, raw)
		return
	}
	const prefix = "/repos/acme/widgets/"
	switch {
	case r.Method != http.MethodGet:
		f.t.Errorf("the bot wrote to the forge's REST API: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.URL.Path == prefix+"pulls/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": "main"}})
	case strings.HasPrefix(r.URL.Path, prefix+"commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"commits/"), "/status")
		var listed []map[string]any
		for _, n := range f.rollup(sha) {
			if n["__typename"] != "StatusContext" {
				continue
			}
			st := f.current(sha, n["context"].(string))
			listed = append(listed, map[string]any{"id": st["id"], "context": st["context"], "state": st["state"],
				"description": st["description"], "target_url": st["target_url"]})
		}
		if listed == nil {
			listed = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "statuses": listed, "total_count": len(listed)})
	case strings.HasPrefix(r.URL.Path, prefix+"commits/") && strings.HasSuffix(r.URL.Path, "/statuses"):
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"commits/"), "/statuses")
		listed := []map[string]any{}
		for i := len(f.statuses[sha]) - 1; i >= 0; i-- {
			listed = append(listed, f.statuses[sha][i])
		}
		_ = json.NewEncoder(w).Encode(listed)
	default:
		f.t.Errorf("the bot asked the forge an unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *gtForge) publish(w http.ResponseWriter, raw []byte) {
	var p struct {
		Summary string         `json:"summary"`
		Gate    map[string]any `json:"gate"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		f.t.Errorf("decode publish payload: %v", err)
	}
	n := len(f.reviews) + 1
	url := reviewURL(n)
	f.reviews = append(f.reviews, map[string]any{"id": n, "user": gtApp(), "body": p.Summary, "html_url": url,
		"lastEditedAt": nil, "editor": nil})
	res := map[string]any{"published": true, "review_url": url, "gate_posted": false}
	if p.Gate != nil {
		pin, _ := p.Gate["audited_sha"].(string)
		if pin != "" && !strings.HasPrefix(f.head, strings.ToLower(pin)) {
			res["gate_error"] = "the head moved since the audit (audited " + pin + ", head is now " + f.head + ")"
		} else {
			state := "success"
			if b, _ := p.Gate["blocking_count"].(float64); b > 0 {
				state = "failure"
			}
			note, _ := p.Gate["note"].(string)
			ctx, _ := p.Gate["context"].(string)
			f.postLocked(f.head, ctx, state, forge.TruncateStatusDescription(note), url, gtApp())
			res["gate_posted"], res["gate_state"], res["gate_sha"] = true, state, f.head
		}
	}
	f.settle()
	_ = json.NewEncoder(w).Encode(res)
}

func gtGraphQLError(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": "UNPROCESSABLE", "message": msg}}})
}

func (f *gtForge) graphql(w http.ResponseWriter, r *http.Request, raw []byte) {
	var q struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		f.t.Errorf("decode GraphQL body: %v", err)
	}
	if msg := assertValidGraphQL(q.Query); strings.Contains(q.Query, "$") && msg != "" {
		f.t.Errorf("invalid GraphQL sent: %s\nquery: %s", msg, q.Query)
	}
	data := func(d map[string]any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": d}) }
	oid, _ := q.Variables["oid"].(string)
	switch {
	case strings.Contains(q.Query, "disablePullRequestAutoMerge"):
		f.autoMerge = ""
		data(map[string]any{"disablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}})
	case strings.Contains(q.Query, "enablePullRequestAutoMerge"):
		switch s := f.state(); {
		case oid != f.head:
			gtGraphQLError(w, "Expected head sha did not match current head ref.")
		case s == "CLEAN":
			gtGraphQLError(w, "Pull request Pull request is in clean status")
		case s == "UNSTABLE":
			gtGraphQLError(w, "Pull request Pull request is in unstable status")
		default:
			f.autoMerge = gtAppLogin
			f.armedOn = append(f.armedOn, oid)
			f.settle()
			data(map[string]any{"enablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}})
		}
	case strings.Contains(q.Query, "mergePullRequest"):
		if oid != f.head {
			gtGraphQLError(w, "Head branch was modified. Review and try the merge again.")
			return
		}
		if s := f.state(); s == "BLOCKED" || s == "BEHIND" {
			gtGraphQLError(w, "Required status checks have not passed ("+s+")")
			return
		}
		f.merged, f.how = oid, "mergePullRequest@"+oid
		data(map[string]any{"mergePullRequest": map[string]any{"clientMutationId": nil}})
	case strings.Contains(q.Query, "enqueuePullRequest"), strings.Contains(q.Query, "dequeuePullRequest"):
		f.t.Errorf("the bot used a merge queue the base does not have: %s", q.Query)
		gtGraphQLError(w, "no merge queue")
	case strings.Contains(q.Query, "statusCheckRollup"):
		data(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"commits": map[string]any{
			"nodes": []any{map[string]any{"commit": map[string]any{"oid": f.head, "statusCheckRollup": map[string]any{
				"contexts": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": f.rollup(f.head)}}}}}}}}})
	case strings.Contains(q.Query, "mergeQueue("):
		data(map[string]any{"repository": map[string]any{"mergeQueue": nil}})
	case strings.Contains(q.Query, "reviews("):
		nodes := []any{}
		for _, rv := range f.reviews {
			nodes = append(nodes, graphQLReview(rv))
		}
		data(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviews": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes}}}})
	case strings.Contains(q.Query, "autoMergeAllowed"):
		if !strings.Contains(r.Header.Get("Accept"), "merge-info-preview") {
			f.t.Errorf("mergeStateStatus queried without the merge-info-preview Accept header")
		}
		pr := map[string]any{"id": "PR_7", "headRefOid": f.head, "baseRefName": "main", "mergeable": "MERGEABLE",
			"mergeStateStatus": f.state(), "autoMergeRequest": nil, "mergeQueueEntry": nil}
		if f.autoMerge != "" {
			pr["autoMergeRequest"] = map[string]any{"enabledAt": "2026-11-10T08:00:00Z", "enabledBy": map[string]any{"login": f.autoMerge}}
		}
		data(map[string]any{"viewer": map[string]any{"login": gtAppLogin},
			"repository": map[string]any{"autoMergeAllowed": true, "pullRequest": pr}})
	case strings.Contains(q.Query, "viewer"):
		data(map[string]any{"viewer": map[string]any{"login": gtAppLogin, "databaseId": vettyID}})
	default:
		f.t.Errorf("the bot asked the forge an unexpected GraphQL query: %s", q.Query)
		gtGraphQLError(w, "unexpected query")
	}
}
