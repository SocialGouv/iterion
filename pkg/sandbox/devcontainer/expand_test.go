package devcontainer

import (
	"testing"
)

func TestExpandLocalVars_LocalEnv(t *testing.T) {
	t.Setenv("HOME", "/home/jo")
	cases := []struct{ in, want string }{
		{"${localEnv:HOME}/.claude", "/home/jo/.claude"},
		{"${localEnv:MISSING}", ""},
		{"${localEnv:MISSING:fallback}", "fallback"},
		{"${localEnv:MISSING:/abs/default}", "/abs/default"},
		{"${localEnv:HOME}/x and ${localEnv:HOME}/y", "/home/jo/x and /home/jo/y"},
		{"plain text", "plain text"},
	}
	for _, c := range cases {
		if got := expandLocalVars(c.in, ""); got != c.want {
			t.Errorf("expandLocalVars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandLocalVars_LocalWorkspaceFolder(t *testing.T) {
	cases := []struct{ in, root, want string }{
		{"${localWorkspaceFolder}/sub", "/repo", "/repo/sub"},
		{"${localWorkspaceFolderBasename}", "/path/to/myproj", "myproj"},
		{"${localWorkspaceFolder}", "", "${localWorkspaceFolder}"}, // no root → leave as-is
	}
	for _, c := range cases {
		if got := expandLocalVars(c.in, c.root); got != c.want {
			t.Errorf("expandLocalVars(%q, root=%q) = %q, want %q", c.in, c.root, got, c.want)
		}
	}
}

func TestExpandLocalVars_LeavesContainerVarsAsIs(t *testing.T) {
	in := "${containerEnv:PATH}/x:${containerWorkspaceFolder}/sub"
	if got := expandLocalVars(in, "/repo"); got != in {
		t.Errorf("expandLocalVars(%q) = %q, want unchanged", in, got)
	}
}

func TestExpandLocalVarsInFile_FullSurface(t *testing.T) {
	t.Setenv("HOME", "/home/jo")
	f := &File{
		Image: "img:${localEnv:VARIANT:default}",
		Mounts: []string{
			"source=${localEnv:HOME}/.claude,target=/home/devbox/.claude,type=bind",
			"source=${localWorkspaceFolder}/cache,target=/cache,type=bind",
		},
		RunArgs: []string{"--security-opt=no-new-privileges=false"},
		ContainerEnv: map[string]string{
			"HOST_WORKSPACE": "${localWorkspaceFolder}",
			// Container-side vars must NOT be expanded here.
			"OUT": "${containerWorkspaceFolder}/dist",
		},
		WorkspaceFolder: "${localWorkspaceFolderBasename}",
		PostCreateCommand: Command{
			Shell: "mkdir -p ${localEnv:HOME}/.claude && cd ${containerWorkspaceFolder}",
		},
	}
	ExpandLocalVarsInFile(f, "/lab/devthefuture/modjo")

	if f.Image != "img:default" {
		t.Errorf("Image = %q", f.Image)
	}
	want0 := "source=/home/jo/.claude,target=/home/devbox/.claude,type=bind"
	if f.Mounts[0] != want0 {
		t.Errorf("Mounts[0] = %q, want %q", f.Mounts[0], want0)
	}
	want1 := "source=/lab/devthefuture/modjo/cache,target=/cache,type=bind"
	if f.Mounts[1] != want1 {
		t.Errorf("Mounts[1] = %q, want %q", f.Mounts[1], want1)
	}
	if f.ContainerEnv["HOST_WORKSPACE"] != "/lab/devthefuture/modjo" {
		t.Errorf("ContainerEnv[HOST_WORKSPACE] = %q", f.ContainerEnv["HOST_WORKSPACE"])
	}
	if f.ContainerEnv["OUT"] != "${containerWorkspaceFolder}/dist" {
		t.Errorf("ContainerEnv[OUT] should be unchanged, got %q", f.ContainerEnv["OUT"])
	}
	if f.WorkspaceFolder != "modjo" {
		t.Errorf("WorkspaceFolder = %q", f.WorkspaceFolder)
	}
	wantShell := "mkdir -p /home/jo/.claude && cd ${containerWorkspaceFolder}"
	if f.PostCreateCommand.Shell != wantShell {
		t.Errorf("PostCreateCommand.Shell = %q, want %q", f.PostCreateCommand.Shell, wantShell)
	}
}
