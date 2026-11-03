package tools

import (
	"os"
	"path/filepath"
	"strings"
)

// CredentialReadGuard decides whether a read target is a credential or
// secret location the host's read tools refuse (iterion#2868). Claw's own
// read tools stay unguarded — the host owns the read scope and calls this
// guard at its gate, the way iterion's scoped read_file/glob/grep/
// read_image do before opening anything.
//
// The exclusion is NOT a frozen name table: it follows the paths where a
// credential is actually read. Three families, all evaluated against the
// path as it is about to be opened (the host passes the resolved,
// symlink-evaluated path; passing the raw spelling too costs nothing and
// covers the pre-resolution alias):
//
//  1. Intrinsic carriers — a credential wherever they appear, the
//     pre-existing doctrine this guard does not reopen: the .env family,
//     .netrc, .git-credentials, cli-auth.json, private keys
//     (id_rsa/id_ed25519, *.pem/*.key/*.p12).
//  2. Real credential LAYOUTS at any depth — the location shapes a tool
//     reads by convention, so a home-like directory held under a read
//     root (a sandboxed runner's HOME, a vendored fixture of a REAL home)
//     is covered wherever it sits: .ssh/, .aws/, .gnupg/, .kube/config,
//     .docker/config.json, .codex/auth.json, .claude/.credentials.json,
//     .config/gh/hosts.yml, .config/gcloud/application_default_credentials.json.
//  3. Derived ABSOLUTE locations — resolved at construction from the
//     process's own environment (KUBECONFIG, NPM_CONFIG_USERCONFIG,
//     NPM_CONFIG_GLOBALCONFIG, CODEX_HOME, AWS_SHARED_CREDENTIALS_FILE,
//     DOCKER_CONFIG, GH_CONFIG_DIR, CLAUDE_CONFIG_DIR, HOME), so the
//     exclusion tracks the credential files this process would actually
//     read, wherever a read root lets the agent reach them — including
//     through a renamed mount or an alias the layouts do not spell.
//
// The consequence the doctrine was chosen for: a file that merely CARRIES
// a credential-looking name outside those locations — a kubeconfig
// template, a fixtures/credentials tree, a testdata auth.json — stays
// readable. The one anchored carrier is .npmrc: npm reads it at a project
// root, so it refuses directly under the workspace or any declared read
// root (and at the derived locations), while a fixtures/ or testdata/
// .npmrc deeper down still reads.
//
// Every comparison is case-folded: a file system that folds case makes
// `.ENV` and `.env` the same file, so a list read in one spelling only is
// a bypass of itself. Folding errs closed — on a case-sensitive file
// system a distinct `.ENV` is refused rather than read, which costs a
// refusal, never a leak.
//
// The verdict is a REFUSAL, never a redaction (the policy iterion#2868
// decided): the read fails with an error the model can act on, and no
// derived bytes leave the gate.
type CredentialReadGuard struct {
	derived []string // folded absolute real credential paths, both lexical and symlink-resolved spellings
	home    string   // folded absolute home directory, "" when the environment names none
}

// NewCredentialReadGuard derives the guard from the process environment.
// A nil env reads os.Getenv; tests pass a map lookup so no process
// environment leaks into the verdict.
func NewCredentialReadGuard(env func(string) string) *CredentialReadGuard {
	if env == nil {
		env = os.Getenv
	}
	g := &CredentialReadGuard{}
	home := strings.TrimSpace(env("HOME"))
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = dir
		}
	}
	if home != "" {
		g.home = foldCredentialPath(home)
		for _, rel := range []string{
			".kube/config",
			".npmrc",
			".codex/auth.json",
			".aws/credentials",
			".docker/config.json",
			".claude/.credentials.json",
			".config/gh/hosts.yml",
			".config/gcloud/application_default_credentials.json",
		} {
			g.addDerived(filepath.Join(home, rel))
		}
	}
	// KUBECONFIG is a filepath list: every entry is a live kubeconfig.
	for _, entry := range filepath.SplitList(env("KUBECONFIG")) {
		g.addDerived(entry)
	}
	for _, v := range []string{"NPM_CONFIG_USERCONFIG", "NPM_CONFIG_GLOBALCONFIG", "AWS_SHARED_CREDENTIALS_FILE"} {
		g.addDerived(env(v))
	}
	for _, pair := range [][2]string{
		{"CODEX_HOME", "auth.json"},
		{"DOCKER_CONFIG", "config.json"},
		{"GH_CONFIG_DIR", "hosts.yml"},
		{"CLAUDE_CONFIG_DIR", ".credentials.json"},
	} {
		if dir := strings.TrimSpace(env(pair[0])); dir != "" {
			g.addDerived(filepath.Join(dir, pair[1]))
		}
	}
	return g
}

// addDerived records one real credential location in both spellings a
// read target can arrive with: the lexical absolute (a raw path spelling
// matches it before any symlink resolution) and the symlink-evaluated one
// (a resolved read target matches it after). The file itself need not
// exist yet — a home directory held through a symlink resolves with the
// credential still unwritten — so the evaluation falls back to the parent
// directory's resolution when the whole path does not evaluate.
func (g *CredentialReadGuard) addDerived(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	g.derived = appendUniqueString(g.derived, foldCredentialPath(abs))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		dir, dirErr := filepath.EvalSymlinks(filepath.Dir(abs))
		if dirErr != nil {
			return
		}
		resolved = filepath.Join(dir, filepath.Base(abs))
	}
	if resolved != abs {
		g.derived = appendUniqueString(g.derived, foldCredentialPath(resolved))
	}
}

// Derived reports the real credential locations the guard resolved at
// construction, case-folded — the witness a host pins so a derivation
// that stops tracking the environment fails loudly.
func (g *CredentialReadGuard) Derived() []string {
	return append([]string(nil), g.derived...)
}

// Refused reports whether path is a credential or secret location. roots
// are the read roots the target was admitted under (the workspace and the
// declared extra roots): they anchor the one project-position carrier,
// .npmrc. An empty path is not refused here — the caller's own required-
// input validation answers it.
func (g *CredentialReadGuard) Refused(path string, roots ...string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	clean := foldCredentialPath(path)
	base := clean[strings.LastIndexByte(clean, '/')+1:]

	// Intrinsic carriers: a credential wherever they appear.
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == ".netrc" || base == ".git-credentials" || base == "cli-auth.json" {
		return true
	}
	if strings.Contains(base, "id_rsa") || strings.Contains(base, "id_ed25519") {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".key", ".p12":
		return true
	}

	// Real credential layouts, at any depth beneath a read root.
	for _, fragment := range []string{
		"/.ssh/", "/.aws/", "/.gnupg/",
		"/.kube/config",
		"/.docker/config.json",
		"/.claude/.credentials.json", "/.codex/auth.json",
		"/.config/gh/hosts.yml",
		"/.config/gcloud/application_default_credentials.json",
	} {
		if strings.Contains("/"+clean, fragment) {
			return true
		}
	}

	// The derived absolute locations this process actually reads.
	for _, d := range g.derived {
		if clean == d {
			return true
		}
	}

	// .npmrc is npm's project-root credential file: refused at the root of
	// the workspace or any declared read root (and at home, via the
	// derived list above) — readable deeper, where it is a fixture.
	if base == ".npmrc" {
		if idx := strings.LastIndexByte(clean, '/'); idx >= 0 {
			parent := clean[:idx]
			if g.home != "" && parent == g.home {
				return true
			}
			for _, root := range roots {
				if foldCredentialPath(root) == parent {
					return true
				}
			}
		}
	}
	return false
}

// foldCredentialPath normalizes a path for comparison: cleaned, slash
// spelled, case-folded. The fold is the whole point — see the guard's
// doc for why a single-spelling list is a bypass of itself.
func foldCredentialPath(path string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
}

func appendUniqueString(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}
