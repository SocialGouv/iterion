package ambient

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAcceptsTheFourValuesOnly(t *testing.T) {
	cases := map[string]struct {
		want Policy
		ok   bool
	}{
		"none":         {None, true},
		"workspace":    {Workspace, true},
		"operator":     {Operator, true},
		"all":          {All, true},
		"  ALL ":       {All, true},
		"Workspace":    {Workspace, true},
		"":             {Workspace, false},
		"project":      {Workspace, false},
		"user,project": {Workspace, false},
		"off":          {Workspace, false},
	}
	for in, c := range cases {
		got, ok := Parse(in)
		if got != c.want || ok != c.ok {
			t.Errorf("Parse(%q) = %v, %v; want %v, %v", in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidateRejectsATypo(t *testing.T) {
	for _, s := range []string{"", "  ", "none", "all", "Operator"} {
		if err := Validate(s); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", s, err)
		}
	}
	err := Validate("workspce")
	if err == nil {
		t.Fatal("Validate(\"workspce\") accepted a typo")
	}
	for _, v := range Values {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("the error %q does not list %q", err, v)
		}
	}
}

func TestResolveSourcedFollowsThePrecedenceChain(t *testing.T) {
	cases := []struct {
		name                             string
		override, node, workflow, envDef string
		want                             Policy
		source                           string
	}{
		{"nothing set", "", "", "", "", Workspace, "default"},
		{"env only", "", "", "", "all", All, "env"},
		{"workflow beats env", "", "", "none", "all", None, "workflow"},
		{"node beats workflow", "", "operator", "none", "all", Operator, "node"},
		{"override beats node", "all", "operator", "none", "none", All, "run_override"},
		{"invalid env never wins", "", "", "", "user,project", Workspace, "default"},
		{"invalid env under a workflow value", "", "", "none", "bogus", None, "workflow"},
		{"blank levels are unset", " ", " ", "", "", Workspace, "default"},
	}
	for _, c := range cases {
		got, src := ResolveSourced(c.override, c.node, c.workflow, c.envDef)
		if got != c.want || src != c.source {
			t.Errorf("%s: ResolveSourced = %v (%s), want %v (%s)", c.name, got, src, c.want, c.source)
		}
	}
}

func TestInvalidEnvFlagsOnlyANonEmptyUnknownValue(t *testing.T) {
	for in, want := range map[string]bool{"": false, " ": false, "all": false, "none": false, "project": true} {
		if got := InvalidEnv(in); got != want {
			t.Errorf("InvalidEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOriginsPerPolicy(t *testing.T) {
	cases := map[Policy][2]bool{ // {workspace, operator}
		None:      {false, false},
		Workspace: {true, false},
		Operator:  {false, true},
		All:       {true, true},
	}
	for p, want := range cases {
		if got := [2]bool{p.IncludesWorkspace(), p.IncludesOperator()}; got != want {
			t.Errorf("%v: origins = %v, want %v", p, got, want)
		}
		if back, ok := Parse(p.String()); !ok || back != p {
			t.Errorf("%v does not round-trip through its spelling %q", p, p.String())
		}
	}
	var zero Policy
	if zero != Workspace {
		t.Fatalf("the zero Policy is %v; it must be the default, Workspace", zero)
	}
}

func TestSupportOfRecordsEachBackendsVerdict(t *testing.T) {
	const H, R, U = Honoured, Refused, Unmeasured
	every := [4]Support{Workspace: H, None: H, Operator: H, All: H}
	cases := map[string][4]Support{
		"claude_code": every,
		"claw":        every,
		"codex":       every,
		"pi":          every,
		"kimi":        {Workspace: H, None: R, Operator: R, All: H},
		"opencode":    {Workspace: R, None: R, Operator: R, All: H},
		"grok":        {Workspace: U, None: R, Operator: R, All: U},
		"":            {U, U, U, U},
		"unknown":     {U, U, U, U},
	}
	for backend, want := range cases {
		for _, p := range []Policy{Workspace, None, Operator, All} {
			got, reason := SupportOf(backend, p)
			if got != want[p] {
				t.Errorf("SupportOf(%q, %v) = %v, want %v", backend, p, got, want[p])
			}
			if (got == Refused) != (reason != "") {
				t.Errorf("SupportOf(%q, %v) reason %q: a refusal says why, nothing else does", backend, p, reason)
			}
			if got := Enforces(backend, p); got != (want[p] == H) {
				t.Errorf("Enforces(%q, %v) = %v", backend, p, got)
			}
		}
	}
}

func TestEnforcingBackendsAndHonouredByAgreeWithTheVerdicts(t *testing.T) {
	if got, want := EnforcingBackends(None), []string{"claude_code", "claw", "codex", "pi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnforcingBackends(none) = %v, want %v", got, want)
	}
	if got, want := EnforcingBackends(All), []string{"claude_code", "claw", "codex", "kimi", "opencode", "pi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnforcingBackends(all) = %v, want %v", got, want)
	}
	for backend, want := range map[string][]string{
		"kimi":     {"workspace", "all"},
		"opencode": {"all"},
		"grok":     nil,
		"codex":    Values,
	} {
		if got := HonouredBy(backend); !reflect.DeepEqual(got, want) {
			t.Errorf("HonouredBy(%q) = %v, want %v", backend, got, want)
		}
	}
}

func TestDeclaredExcludesOnlyTheDefault(t *testing.T) {
	for source, want := range map[string]bool{"": false, "default": false, "env": true, "workflow": true, "node": true, "run_override": true} {
		if got := Declared(source); got != want {
			t.Errorf("Declared(%q) = %v, want %v", source, got, want)
		}
	}
}

func TestRemedyNamesTheWaysOut(t *testing.T) {
	got := Remedy("kimi", None, "kimi reads the walk")
	for _, want := range []string{"kimi reads the walk", "workspace, all", "claude_code, claw, codex, pi", "ADR-119"} {
		if !strings.Contains(got, want) {
			t.Errorf("Remedy = %q, missing %q", got, want)
		}
	}
	if got := Remedy("grok", None, "r"); !strings.Contains(got, "drop the declared value") {
		t.Errorf("Remedy for a backend that applies no policy = %q, want it to suggest dropping the value", got)
	}
}

func TestRepoRootFindsTheNearestRealGitEntry(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "pkg", "x")
	mustGitDir(t, filepath.Join(repo, ".git"))
	mustMkdir(t, sub)

	if got := RepoRoot(sub); got != repo {
		t.Errorf("RepoRoot(%s) = %s, want the primary checkout %s", sub, got, repo)
	}

	// A linked worktree nested in the repository is its own root: its .git is
	// a "gitdir:" file.
	wt := filepath.Join(repo, ".iterion", "worktrees", "run1")
	mustMkdir(t, filepath.Join(wt, "src"))
	mustWrite(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "run1")+"\n")
	if got := RepoRoot(filepath.Join(wt, "src")); got != wt {
		t.Errorf("RepoRoot inside a linked worktree = %s, want %s", got, wt)
	}

	// Nothing below is a repository, so the walks exhaust the ancestors of
	// tmp: they answer the work dir itself only when the host carries no git
	// entry above it.
	requireNoGitAncestor(t, tmp)

	// An empty placeholder .git file is not a repository.
	plain := filepath.Join(tmp, "plain")
	mustMkdir(t, filepath.Join(plain, "deep"))
	mustWrite(t, filepath.Join(plain, ".git"), "")
	if got := RepoRoot(filepath.Join(plain, "deep")); got != filepath.Join(plain, "deep") {
		t.Errorf("RepoRoot under an empty .git placeholder = %s, want the work dir itself", got)
	}

	// Outside any repository the root is the work dir.
	lone := filepath.Join(tmp, "lone")
	mustMkdir(t, lone)
	if got := RepoRoot(lone); got != lone {
		t.Errorf("RepoRoot outside a repository = %s, want %s", got, lone)
	}
}

func TestRepoRootWalksPastAnEmptyGitDirectory(t *testing.T) {
	tmp := t.TempDir()

	// git's own discovery climbs past a `.git` directory that is not a
	// repository — no HEAD, no objects/, no refs/ — and answers the real
	// repository above it; RepoRoot must agree.
	repo := filepath.Join(tmp, "repo")
	mustGitDir(t, filepath.Join(repo, ".git"))
	stray := filepath.Join(repo, "stray")
	work := filepath.Join(stray, "deep")
	mustMkdir(t, filepath.Join(stray, ".git"))
	mustMkdir(t, work)
	if got := RepoRoot(work); got != repo {
		t.Errorf("RepoRoot under an empty .git directory = %s, want the real repository %s", got, repo)
	}

	// With no repository anywhere above, the root is the work dir itself —
	// the answer git's "not a git repository" maps to. The assertion depends
	// on the host carrying no git entry above tmp.
	requireNoGitAncestor(t, tmp)
	hostless := filepath.Join(tmp, "hostless", ".git")
	mustMkdir(t, hostless)
	deep := filepath.Join(tmp, "hostless", "deep")
	mustMkdir(t, deep)
	if got := RepoRoot(deep); got != deep {
		t.Errorf("RepoRoot under an empty .git directory with no repository above = %s, want the work dir itself", got)
	}
}

// TestRepoRootHoldsAGitDirectoryToGitsCriteria pins each criterion of
// isGitDirectory individually: mutate any one of them and its sub-case
// reddens. "repository" means RepoRoot answers the stray .git's directory;
// "not a repository" means the walk climbs past it and answers the work dir.
func TestRepoRootHoldsAGitDirectoryToGitsCriteria(t *testing.T) {
	validRef := "ref: refs/heads/main\n"
	hexOID := "0123456789abcdef0123456789abcdef01234567\n"
	cases := []struct {
		name string
		head *string // nil: no HEAD at all
		// symlink, when set, replaces the HEAD content with a link to it
		symlink               string
		objects, refs, isRepo bool
	}{
		{name: "entirely empty", isRepo: false},
		{name: "HEAD alone, no objects or refs", head: &validRef, isRepo: false},
		{name: "HEAD and refs, no objects", head: &validRef, refs: true, isRepo: false},
		{name: "empty HEAD with objects and refs", head: new(string), objects: true, refs: true, isRepo: false},
		{name: "garbage HEAD with objects and refs", head: ptr("garbage\n"), objects: true, refs: true, isRepo: false},
		{name: "dangling HEAD symlink with objects and refs", symlink: "refs/heads/gone", objects: true, refs: true, isRepo: true},
		{name: "valid ref HEAD with objects and refs", head: &validRef, objects: true, refs: true, isRepo: true},
		{name: "hex oid HEAD with objects and refs", head: &hexOID, objects: true, refs: true, isRepo: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			requireNoGitAncestor(t, tmp)
			stray := filepath.Join(tmp, "stray")
			work := filepath.Join(stray, "deep")
			mustMkdir(t, work)
			dotGit := filepath.Join(stray, ".git")
			mustMkdir(t, dotGit)
			if c.objects {
				mustMkdir(t, filepath.Join(dotGit, "objects"))
			}
			if c.refs {
				mustMkdir(t, filepath.Join(dotGit, "refs"))
			}
			if c.symlink != "" {
				if err := os.Symlink(c.symlink, filepath.Join(dotGit, "HEAD")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if c.head != nil {
				mustWrite(t, filepath.Join(dotGit, "HEAD"), *c.head)
			}
			want := work
			if c.isRepo {
				want = stray
			}
			if got := RepoRoot(work); got != want {
				t.Errorf("RepoRoot(%s) = %s, want %s", work, got, want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// mustGitDir plants the shape git's is_git_directory requires of a `.git`
// directory: a HEAD that reads, and objects/ and refs/ directories.
func mustGitDir(t *testing.T, dotGit string) {
	t.Helper()
	mustMkdir(t, filepath.Join(dotGit, "objects"))
	mustMkdir(t, filepath.Join(dotGit, "refs"))
	mustWrite(t, filepath.Join(dotGit, "HEAD"), "ref: refs/heads/main\n")
}

// requireNoGitAncestor skips the test when an ancestor of dir carries a
// `.git` entry: an assertion whose walk climbs out of dir would stop at that
// host repository instead of exhausting the walk, so its premise is the
// host's, not the test's.
func requireNoGitAncestor(t *testing.T, dir string) {
	t.Helper()
	for _, ancestor := range AncestorsAbove(dir) {
		if _, err := os.Lstat(filepath.Join(ancestor, ".git")); err == nil {
			t.Skipf("host-dependent: %s carries a .git entry, so a walk out of %s stops there", ancestor, dir)
		}
	}
}

func TestAncestorsAboveStopsAtTheFilesystemRoot(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "a", "b", "c")
	want := []string{
		filepath.Join(string(filepath.Separator), "a", "b"),
		filepath.Join(string(filepath.Separator), "a"),
		string(filepath.Separator),
	}
	if got := AncestorsAbove(root); !reflect.DeepEqual(got, want) {
		t.Errorf("AncestorsAbove(%s) = %v, want %v", root, got, want)
	}
	if got := AncestorsAbove(string(filepath.Separator)); len(got) != 0 {
		t.Errorf("AncestorsAbove(/) = %v, want none", got)
	}
}

func TestFormsAddsTheResolvedSpelling(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	mustMkdir(t, real)
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got := Forms(link)
	if len(got) != 2 || got[0] != link || got[1] != resolvedReal {
		t.Errorf("Forms(%s) = %v, want [%s %s]", link, got, link, resolvedReal)
	}
	if got := Forms(resolvedReal); len(got) != 1 {
		t.Errorf("Forms of an already-resolved path = %v, want one form", got)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNestedWorktreeMainFollowsGitsOwnLayout(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	mustMkdir(t, main)
	run(main, "init", "-q", "-b", "main")
	run(main, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	nested := filepath.Join(main, ".iterion", "worktrees", "run1")
	run(main, "worktree", "add", "-q", "--detach", nested)
	sibling := filepath.Join(tmp, "sibling")
	run(main, "worktree", "add", "-q", "--detach", sibling)

	resolvedMain, _ := filepath.EvalSymlinks(main)
	if got, ok := NestedWorktreeMain(nested); !ok || resolved(got) != resolvedMain {
		t.Errorf("NestedWorktreeMain(nested) = %q, %v; want %s, true", got, ok, main)
	}
	if got := OperatorBoundary(RepoRoot(nested)); resolved(got) != resolvedMain {
		t.Errorf("OperatorBoundary(nested) = %s, want the main checkout %s", got, main)
	}
	for name, dir := range map[string]string{"sibling worktree": sibling, "main checkout": main} {
		if got, ok := NestedWorktreeMain(dir); ok {
			t.Errorf("%s: NestedWorktreeMain = %q, true; want not nested", name, got)
		}
		if got := OperatorBoundary(dir); got != dir {
			t.Errorf("%s: OperatorBoundary = %s, want the root itself", name, got)
		}
	}
	placeholder := filepath.Join(tmp, "placeholder")
	mustMkdir(t, placeholder)
	mustWrite(t, filepath.Join(placeholder, ".git"), "")
	if _, ok := NestedWorktreeMain(placeholder); ok {
		t.Error("an empty .git placeholder read as a nested worktree")
	}
}
