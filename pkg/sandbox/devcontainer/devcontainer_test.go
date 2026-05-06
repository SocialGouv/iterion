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
	spec := ToSandboxSpec(f)
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
	spec := ToSandboxSpec(f)
	if spec.User != "alice" {
		t.Errorf("User = %q, want alice (containerUser fallback)", spec.User)
	}
}

func TestToSandboxSpecPostCreateArrayJoined(t *testing.T) {
	f := &File{Image: "x", PostCreateCommand: Command{Argv: []string{"npm", "ci"}}}
	spec := ToSandboxSpec(f)
	if spec.PostCreate != "npm ci" {
		t.Errorf("PostCreate = %q", spec.PostCreate)
	}
}
