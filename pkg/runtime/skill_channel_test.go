package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// writeSkill lays a skill file down: dir form ("name/SKILL.md") or flat
// ("name.md"), under dir.
func writeSkill(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The channel root re-carries the owned skills and ONLY them, in the
// .claude/skills layout an --add-dir backend discovers — and it is reset,
// never read: a directory of the same name the checkout shipped is replaced.
func TestMirrorSkillChannel(t *testing.T) {
	workDir := t.TempDir()
	mirrored := filepath.Join(workDir, ".claude", "skills")
	dirSkill := writeSkill(t, mirrored, "review/SKILL.md", "---\ndescription: owned\n---\nReview owned.\n")
	flatSkill := writeSkill(t, mirrored, "deploy.md", "Deploy body.\n")
	// The checkout's own skill, mirrored-never: it must not enter the channel.
	writeSkill(t, mirrored, "planted/SKILL.md", "planted\n")
	// A directory of the channel's own name, committed by the checkout.
	writeSkill(t, skillChannelRoot(workDir), ".claude/skills/review/SKILL.md", "planted twin\n")

	owned := []string{filepath.Join(mirrored, "review"), flatSkill}
	root, allowlist, err := mirrorSkillChannel(workDir, owned, nil)
	if err != nil {
		t.Fatalf("mirrorSkillChannel: %v", err)
	}
	if root != skillChannelRoot(workDir) {
		t.Errorf("root = %q, want %q", root, skillChannelRoot(workDir))
	}
	if len(allowlist) != 2 {
		t.Fatalf("allowlist = %v, want exactly the two owned skills", allowlist)
	}
	if got, want := allowlist["review"], filepath.Join(root, ".claude", "skills", "review", "SKILL.md"); got != want {
		t.Errorf("allowlist[review] = %q, want %q", got, want)
	}
	if got, want := allowlist["deploy"], filepath.Join(root, ".claude", "skills", "deploy.md"); got != want {
		t.Errorf("allowlist[deploy] = %q, want %q", got, want)
	}
	// The pinned file carries the owned bytes, and the planted twin the
	// checkout had at the channel's own path is gone.
	body, err := os.ReadFile(allowlist["review"])
	if err != nil || !strings.Contains(string(body), "Review owned.") {
		t.Errorf("the channel's review skill = (%q, %v), want the owned bytes", body, err)
	}
	if strings.Contains(string(body), "planted") {
		t.Error("the checkout's pre-existing channel directory survived the reset")
	}
	if _, ok := allowlist["planted"]; ok {
		t.Error("a skill the run does not own entered the allowlist")
	}
	// The owned source is untouched by the copy.
	if _, err := os.Stat(dirSkill); err != nil {
		t.Errorf("the mirrored source vanished: %v", err)
	}
}

// First owner wins: owned arrives in mirror precedence order (bundle >
// plugin > library), and a name a later tier also owns is not re-decided.
func TestMirrorSkillChannel_FirstOwnerOfANameWins(t *testing.T) {
	workDir := t.TempDir()
	mirrored := filepath.Join(workDir, ".claude", "skills")
	first := writeSkill(t, mirrored, "review/SKILL.md", "bundle's review\n")
	second := writeSkill(t, mirrored, "review.md", "library's review\n")

	_, allowlist, err := mirrorSkillChannel(workDir, []string{filepath.Join(mirrored, "review"), second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(allowlist["review"])
	if !strings.Contains(string(body), "bundle's review") {
		t.Errorf("the name resolved to %q, want the first owner's bytes (%q)", body, first)
	}
}

type recordingChannelExecutor struct {
	NodeExecutor
	allowlist   map[string]string
	armed       bool
	channelRoot string
}

func (r *recordingChannelExecutor) SetSkillAllowlist(m map[string]string) {
	r.allowlist, r.armed = m, m != nil
}

func (r *recordingChannelExecutor) SetSkillChannelRoot(dir string) { r.channelRoot = dir }

// The trust-keyed arming: a trusted run keeps the workspace walk, an
// untrusted one — and an UNKNOWN trust value, which reads untrusted by
// construction (store.RunTrust.Trusted) — is armed with exactly the owned
// set, fail-closed.
func TestApplyMirroredSkills_ArmsTheChannelByTrust(t *testing.T) {
	workDir := t.TempDir()
	writeSkill(t, filepath.Join(workDir, ".claude", "skills"), "review/SKILL.md", "owned\n")
	owned := []string{filepath.Join(workDir, ".claude", "skills", "review")}

	trusted := &recordingChannelExecutor{}
	(&Engine{executor: trusted, workDir: workDir, trust: store.RunTrustDefault}).applyMirroredSkills(owned)
	if trusted.armed || trusted.channelRoot != "" {
		t.Errorf("a trusted run armed the channel: %v, %q", trusted.allowlist, trusted.channelRoot)
	}

	for _, trust := range []store.RunTrust{store.RunTrustFork, store.RunTrust("vendored")} {
		rec := &recordingChannelExecutor{}
		(&Engine{executor: rec, workDir: workDir, trust: trust}).applyMirroredSkills(owned)
		if !rec.armed {
			t.Fatalf("trust=%q: the channel is not armed — the run would walk the checkout's roots", trust)
		}
		if rec.channelRoot != skillChannelRoot(workDir) {
			t.Errorf("trust=%q: channel root = %q", trust, rec.channelRoot)
		}
		if _, ok := rec.allowlist["review"]; !ok || len(rec.allowlist) != 1 {
			t.Errorf("trust=%q: allowlist = %v, want exactly the owned skill", trust, rec.allowlist)
		}
	}

	// An untrusted run that owns NO skill arms the empty set: the `skill`
	// tool refuses everything rather than falling back to the walk.
	rec := &recordingChannelExecutor{}
	(&Engine{executor: rec, workDir: t.TempDir(), trust: store.RunTrustFork}).applyMirroredSkills(nil)
	if !rec.armed || len(rec.allowlist) != 0 {
		t.Errorf("no owned skills = (armed %v, %v), want armed with the empty set", rec.armed, rec.allowlist)
	}
}
