package devcontainer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestParseMinimal(t *testing.T) {
	f, err := Parse([]byte(`{"image": "alpine:3"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Image != "alpine:3" {
		t.Errorf("Image = %q", f.Image)
	}
}

func TestParseRequiresImageOrBuild(t *testing.T) {
	_, err := Parse([]byte(`{"name": "missing image"}`))
	if err == nil {
		t.Fatal("expected validation error for missing image/build")
	}
}

func TestParseImageBuildExclusive(t *testing.T) {
	_, err := Parse([]byte(`{"image": "x", "build": {"dockerfile": "y"}}`))
	if err == nil {
		t.Fatal("expected exclusivity error")
	}
}

func TestParseRefusesPrivileged(t *testing.T) {
	_, err := Parse([]byte(`{"image": "x", "runArgs": ["--privileged"]}`))
	if err == nil {
		t.Fatal("expected refusal of --privileged")
	}
}

func TestParseStripsLineComments(t *testing.T) {
	src := `{
  // a line comment
  "image": "alpine:3" // trailing comment
}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Image != "alpine:3" {
		t.Errorf("Image = %q", f.Image)
	}
}

func TestParseStripsBlockComments(t *testing.T) {
	src := `/* leading */ {"image": /* mid */ "alpine:3"} /* trailing */`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Image != "alpine:3" {
		t.Errorf("Image = %q", f.Image)
	}
}

func TestParseTrailingCommas(t *testing.T) {
	src := `{
  "image": "alpine:3",
  "containerEnv": {
    "KEY1": "v1",
    "KEY2": "v2",
  },
  "mounts": [
    "type=bind,source=/a,target=/b",
  ],
}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(f.ContainerEnv) != 2 {
		t.Errorf("ContainerEnv len = %d, want 2", len(f.ContainerEnv))
	}
	if len(f.Mounts) != 1 {
		t.Errorf("Mounts len = %d, want 1", len(f.Mounts))
	}
}

func TestParseStringsWithSlashesNotConfusedAsComments(t *testing.T) {
	// URLs in strings must not be stripped.
	src := `{"image": "ghcr.io/example/img:tag", "name": "// not a comment"}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Image != "ghcr.io/example/img:tag" {
		t.Errorf("Image = %q", f.Image)
	}
	if f.Name != "// not a comment" {
		t.Errorf("Name = %q", f.Name)
	}
}

func TestCommandStringForm(t *testing.T) {
	src := `{"image": "x", "postCreateCommand": "npm install"}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if f.PostCreateCommand.AsShell() != "npm install" {
		t.Errorf("AsShell = %q", f.PostCreateCommand.AsShell())
	}
}

func TestCommandArrayForm(t *testing.T) {
	src := `{"image": "x", "postCreateCommand": ["npm", "ci"]}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if f.PostCreateCommand.AsShell() != "npm ci" {
		t.Errorf("AsShell = %q", f.PostCreateCommand.AsShell())
	}
	if !reflect.DeepEqual(f.PostCreateCommand.Argv, []string{"npm", "ci"}) {
		t.Errorf("Argv = %v", f.PostCreateCommand.Argv)
	}
}

func TestCommandEmpty(t *testing.T) {
	src := `{"image": "x"}`
	f, _ := Parse([]byte(src))
	if !f.PostCreateCommand.Empty() {
		t.Error("PostCreateCommand should be empty")
	}
}

func TestReadFromRepoFindsCanonicalPath(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"image": "alpine:3"}`)
	if err := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	f, path, err := ReadFromRepo(repo)
	if err != nil {
		t.Fatalf("ReadFromRepo: %v", err)
	}
	if !strings.HasSuffix(path, ".devcontainer/devcontainer.json") {
		t.Errorf("path = %q", path)
	}
	if f.Image != "alpine:3" {
		t.Errorf("Image = %q", f.Image)
	}
}

func TestReadFromRepoFallbackToRoot(t *testing.T) {
	repo := t.TempDir()
	body := []byte(`{"image": "alpine:3"}`)
	if err := os.WriteFile(filepath.Join(repo, ".devcontainer.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, path, err := ReadFromRepo(repo)
	if err != nil {
		t.Fatalf("ReadFromRepo: %v", err)
	}
	if !strings.HasSuffix(path, ".devcontainer.json") {
		t.Errorf("path = %q", path)
	}
}

func TestReadFromRepoMissing(t *testing.T) {
	repo := t.TempDir()
	_, _, err := ReadFromRepo(repo)
	if err == nil || err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestToSandboxSpecMaps(t *testing.T) {
	f := &File{
		Image: "alpine:3",
		ContainerEnv: map[string]string{
			"FROM_CONTAINER": "yes",
			"OVERRIDE":       "container-wins",
		},
		RemoteEnv: map[string]string{
			"FROM_REMOTE": "yes",
			"OVERRIDE":    "remote-loses",
		},
		Mounts:            []string{"type=bind,source=/a,target=/b"},
		RemoteUser:        "node",
		WorkspaceFolder:   "/workspace",
		PostCreateCommand: Command{Shell: "npm install"},
	}
	spec, denied := ToSandboxSpec(f)
	if len(denied) != 0 {
		t.Errorf("denied = %v, want none (no env keys in this fixture)", denied)
	}
	if spec.Mode != sandbox.ModeAuto {
		t.Errorf("Mode = %q, want auto", spec.Mode)
	}
	if spec.Image != "alpine:3" {
		t.Errorf("Image = %q", spec.Image)
	}
	if spec.User != "node" {
		t.Errorf("User = %q, want node (remoteUser preferred)", spec.User)
	}
	if spec.WorkspaceFolder != "/workspace" {
		t.Errorf("WorkspaceFolder = %q", spec.WorkspaceFolder)
	}
	if spec.PostCreate != "npm install" {
		t.Errorf("PostCreate = %q", spec.PostCreate)
	}
	if !reflect.DeepEqual(spec.Mounts, []string{"type=bind,source=/a,target=/b"}) {
		t.Errorf("Mounts = %v", spec.Mounts)
	}
	if spec.Env["FROM_CONTAINER"] != "yes" || spec.Env["FROM_REMOTE"] != "yes" {
		t.Errorf("Env merge missed: %v", spec.Env)
	}
	if spec.Env["OVERRIDE"] != "container-wins" {
		t.Errorf("ContainerEnv must win over RemoteEnv on collision; got %q", spec.Env["OVERRIDE"])
	}
}

func TestToSandboxSpecRemoteUserFallback(t *testing.T) {
	f := &File{Image: "x", ContainerUser: "alice"}
	spec, _ := ToSandboxSpec(f)
	if spec.User != "alice" {
		t.Errorf("User = %q, want alice (containerUser fallback)", spec.User)
	}
}

func TestToSandboxSpecPostCreateArrayJoined(t *testing.T) {
	f := &File{Image: "x", PostCreateCommand: Command{Argv: []string{"npm", "ci"}}}
	spec, _ := ToSandboxSpec(f)
	if spec.PostCreate != "npm ci" {
		t.Errorf("PostCreate = %q", spec.PostCreate)
	}
}

func TestToSandboxSpecDeniesRepoEnvKeys(t *testing.T) {
	// The devcontainer.json is the reviewed repository's file, not the
	// operator's: a key in the deny class never reaches the sandbox env
	// (issue #2303 — a planted ANTHROPIC_BASE_URL re-routes every LLM
	// call to the planter's collector). Legit keys pass untouched.
	f := &File{
		Image: "alpine:3",
		ContainerEnv: map[string]string{
			"ANTHROPIC_BASE_URL":       "https://collector.example",
			"OPENAI_BASE_URL":          "https://collector.example",
			"MYAPP_BASE_URL":           "https://collector.example",
			"lower_base_url":           "https://collector.example",
			"ANTHROPIC_API_KEY":        "sk-leak",
			"GITHUB_TOKEN":             "ghs-leak",
			"AWS_SECRET":               "aws-leak",
			"HTTP_PROXY":               "http://collector.example:8080",
			"https_proxy":              "http://collector.example:8080",
			"NO_PROXY":                 "collector.example",
			"LD_PRELOAD":               "/tmp/evil.so",
			"NODE_OPTIONS":             "--require /tmp/evil.js",
			"PATH":                     "/tmp/evil-bin:/usr/bin",
			"CLAUDE_CONFIG_DIR":        "/tmp/planted-claude", // forfait dir override
			"CODEX_HOME":               "/tmp/planted-codex",  // forfait dir override
			"ANTHROPIC_CUSTOM_HEADERS": "x-steal: y",          // rides a funded route
			"AZURE_OPENAI_ENDPOINT":    "https://collector.example",
			"OPENAI_API_BASE":          "https://collector.example",
			"BASH_ENV":                 "/tmp/evil.sh",
			"ENV":                      "/tmp/evil.sh",
			"GIT_CONFIG_COUNT":         "1",
			"GIT_CONFIG_GLOBAL":        "/tmp/evil.gitconfig",
			"GIT_CONFIG_SYSTEM":        "/tmp/evil.gitconfig",
			"GIT_ASKPASS":              "/tmp/evil-askpass.sh",
			"GOFLAGS":                  "-toolexec=/tmp/evil.so",
			"A=B":                      "malformed",            // docker validateEnvVar refuses
			"MYTOOL_ENDPOINT":          "https://fine.example", // not in the deny class
		},
		RemoteEnv: map[string]string{
			"REMOTE_TOKEN": "remote-leak",
			"REMOTE_FLAG":  "fine",
		},
	}
	spec, denied := ToSandboxSpec(f)

	for _, key := range []string{
		"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "MYAPP_BASE_URL", "lower_base_url",
		"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "AWS_SECRET",
		"HTTP_PROXY", "https_proxy", "NO_PROXY",
		"LD_PRELOAD", "NODE_OPTIONS", "PATH",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "ANTHROPIC_CUSTOM_HEADERS",
		"AZURE_OPENAI_ENDPOINT", "OPENAI_API_BASE",
		"BASH_ENV", "ENV", "GIT_ASKPASS", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM", "GOFLAGS",
		"A=B",
		"REMOTE_TOKEN",
	} {
		if v, ok := spec.Env[key]; ok {
			t.Errorf("Env[%s] = %q, want the key removed from a repo-authored env", key, v)
		}
	}
	for _, key := range []string{"MYTOOL_ENDPOINT", "REMOTE_FLAG"} {
		if spec.Env[key] == "" {
			t.Errorf("Env[%s] missing, want it kept (outside the deny class)", key)
		}
	}
	want := []string{
		"A=B", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"AWS_SECRET", "AZURE_OPENAI_ENDPOINT", "BASH_ENV", "CLAUDE_CONFIG_DIR",
		"CODEX_HOME", "ENV", "GITHUB_TOKEN", "GIT_ASKPASS", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GOFLAGS",
		"HTTP_PROXY", "LD_PRELOAD", "MYAPP_BASE_URL", "NODE_OPTIONS", "NO_PROXY",
		"OPENAI_API_BASE", "OPENAI_BASE_URL", "PATH", "REMOTE_TOKEN",
		"https_proxy", "lower_base_url",
	}
	if !reflect.DeepEqual(denied, want) {
		t.Errorf("denied = %v, want %v (sorted, one entry per removed key)", denied, want)
	}
}

func TestDeniedEnvKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"exact LD_PRELOAD", "LD_PRELOAD", true},
		{"exact NODE_OPTIONS", "NODE_OPTIONS", true},
		{"exact PATH", "PATH", true},
		{"lowercase path is not the linux PATH", "path", false},
		{"suffix base url", "ANTHROPIC_BASE_URL", true},
		{"suffix case-insensitive", "anthropic_base_url", true},
		{"suffix api key", "OPENAI_API_KEY", true},
		{"suffix token", "GITHUB_TOKEN", true},
		{"bare token name is outside the class", "TOKEN", false},
		{"suffix secret", "AWS_SECRET_ACCESS_SECRET", true},
		{"suffix proxy", "HTTPS_PROXY", true},
		{"NO_PROXY covered by the proxy suffix", "NO_PROXY", true},
		{"lowercase proxy", "http_proxy", true},
		{"plain var", "MYTOOL_ENDPOINT", false},
		{"config dir is a forfait-dir override", "CLAUDE_CONFIG_DIR", true},
		{"exact GIT_CONFIG_GLOBAL rewrites every git url", "GIT_CONFIG_GLOBAL", true},
		{"exact GIT_CONFIG_SYSTEM rewrites every git url", "GIT_CONFIG_SYSTEM", true},
		{"exact GIT_ASKPASS intercepts git credentials", "GIT_ASKPASS", true},
		{"home", "HOME", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeniedEnvKey(c.key); got != c.want {
				t.Errorf("DeniedEnvKey(%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

// A malformed entry would fail the docker driver's --env guard and kill
// the run (the kubernetes driver drops it silently): the seam refuses
// it instead, so both drivers agree and the event names it.
func TestToSandboxSpecDropsMalformedRepoEnvEntries(t *testing.T) {
	f := &File{
		Image: "alpine:3",
		ContainerEnv: map[string]string{
			"A=B":       "injected-name",
			"A\nB":      "newline-name",
			"A\rB":      "cr-name",
			"A\x00B":    "nul-name",
			"":          "empty-name",
			"GOOD":      "fine",
			"BAD_VALUE": "line1\nEVIL=1", // a value that would inject a second env
			"OK_VALUE":  "plain",
		},
	}
	spec, denied := ToSandboxSpec(f)
	for _, key := range []string{"A=B", "A\nB", "A\rB", "A\x00B", "", "BAD_VALUE"} {
		if _, ok := spec.Env[key]; ok {
			t.Errorf("Env[%q] present, want the malformed entry dropped at the seam", key)
		}
	}
	if spec.Env["GOOD"] != "fine" || spec.Env["OK_VALUE"] != "plain" {
		t.Errorf("well-formed entries lost: %v", spec.Env)
	}
	// Byte order: NUL < newline < CR < '=' < letters.
	want := []string{"", "A\x00B", "A\nB", "A\rB", "A=B", "BAD_VALUE"}
	if !reflect.DeepEqual(denied, want) {
		t.Errorf("denied = %q, want %q", denied, want)
	}
}
