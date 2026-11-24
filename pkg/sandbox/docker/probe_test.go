package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestClassifyImageResolve(t *testing.T) {
	cases := []struct {
		name        string
		out         string
		err         error
		wantNil     bool
		wantTransit bool
	}{
		{name: "success", out: "{...manifest...}", err: nil, wantNil: true},
		{name: "manifest unknown -> fail", out: "manifest unknown", err: errors.New("exit 1"), wantTransit: false},
		{name: "no such manifest -> fail", out: "no such manifest: ghcr.io/x/y:bad", err: errors.New("exit 1"), wantTransit: false},
		{name: "not found -> fail", out: "manifest for ghcr.io/x/y:bad not found", err: errors.New("exit 1"), wantTransit: false},
		{name: "unauthorized -> transient", out: "unauthorized: authentication required", err: errors.New("exit 1"), wantTransit: true},
		{name: "network -> transient", out: "dial tcp: lookup ghcr.io: no such host", err: errors.New("exit 1"), wantTransit: true},
		// Auth denials phrased with "does not exist" / "not found" must
		// stay transient (the daemon may hold creds the probe lacks), not
		// be misread as a genuinely-absent tag.
		{name: "access-denied with 'does not exist' -> transient", out: "pull access denied for ghcr.io/org/private, repository does not exist or may require 'docker login': denied", err: errors.New("exit 1"), wantTransit: true},
		{name: "unknown failure -> transient (safe default)", out: "some weird registry hiccup", err: errors.New("exit 1"), wantTransit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyImageResolve("ghcr.io/x/y:bad", []byte(tc.out), tc.err)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("classifyImageResolve() = %v, want nil", got)
				}
				return
			}
			var ire *ImageResolveError
			if !errors.As(got, &ire) {
				t.Fatalf("classifyImageResolve() = %v, want *ImageResolveError", got)
			}
			if ire.Transient != tc.wantTransit {
				t.Errorf("Transient = %v, want %v (err text: %q)", ire.Transient, tc.wantTransit, ire.Error())
			}
		})
	}
}

func TestValidateSpecMounts(t *testing.T) {
	cases := []struct {
		name    string
		spec    sandbox.Spec
		wantErr bool
	}{
		{name: "clean", spec: sandbox.Spec{Image: "alpine:3", Mounts: []string{"type=bind,source=/home/u/proj,target=/work"}}},
		{name: "docker.sock blocked", spec: sandbox.Spec{Image: "alpine:3", Mounts: []string{"type=bind,source=/var/run/docker.sock,target=/x"}}, wantErr: true},
		{name: "ssh dir blocked", spec: sandbox.Spec{Image: "alpine:3", Mounts: []string{"type=bind,source=/home/u/.ssh,target=/x"}}, wantErr: true},
		{name: "flag injection image", spec: sandbox.Spec{Image: "-rm"}, wantErr: true},
		{name: "control char user", spec: sandbox.Spec{Image: "alpine:3", User: "1000\n--privileged"}, wantErr: true},
		{name: "env injection value", spec: sandbox.Spec{Image: "alpine:3", Env: map[string]string{"K": "v\nDOCKER_HOST=tcp://evil"}}, wantErr: true},
		{name: "env bad key", spec: sandbox.Spec{Image: "alpine:3", Env: map[string]string{"BAD=KEY": "v"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpecMounts(tc.spec)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateSpecMounts() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestPingDaemonStub exercises the daemon-liveness classification via the
// runDockerProbe seam. It requires a runtime binary on PATH (Detect runs
// before the seam); skips otherwise, mirroring docker integration tests.
func TestPingDaemonStub(t *testing.T) {
	if _, err := Detect(); err != nil {
		t.Skipf("no container runtime on PATH: %v", err)
	}
	old := runDockerProbe
	defer func() { runDockerProbe = old }()

	t.Run("daemon down", func(t *testing.T) {
		runDockerProbe = func(ctx context.Context, cli CLI, env []string, args ...string) ([]byte, error) {
			return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock."), errors.New("exit status 1")
		}
		_, err := PingDaemon(context.Background())
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("PingDaemon() = %v, want unreachable error", err)
		}
	})

	t.Run("daemon up", func(t *testing.T) {
		runDockerProbe = func(ctx context.Context, cli CLI, env []string, args ...string) ([]byte, error) {
			return []byte("27.1.1\n"), nil
		}
		v, err := PingDaemon(context.Background())
		if err != nil {
			t.Fatalf("PingDaemon() = %v, want nil", err)
		}
		if v != "27.1.1" {
			t.Errorf("server version = %q, want 27.1.1", v)
		}
	})

	t.Run("empty version", func(t *testing.T) {
		runDockerProbe = func(ctx context.Context, cli CLI, env []string, args ...string) ([]byte, error) {
			return []byte("\n"), nil
		}
		if _, err := PingDaemon(context.Background()); err == nil {
			t.Fatal("PingDaemon() should error on empty server version")
		}
	})
}
