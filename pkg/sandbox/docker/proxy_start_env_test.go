package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestStartProxyEnvironmentStaysOffArgvAndClientEnv(t *testing.T) {
	for _, mode := range []string{"", "once", "always"} {
		t.Run("conflict="+mode, func(t *testing.T) {
			cli, log := fakeDaemon(t)
			t.Setenv("FAKE_DAEMON_CONFLICT", mode)
			t.Setenv("HTTP_PROXY", "http://host-original.invalid:3128")
			t.Setenv("TMPDIR", t.TempDir())
			evidence := filepath.Join(t.TempDir(), "env-file")
			t.Setenv("PROXY_ENV_EVIDENCE", evidence)
			raw, err := os.ReadFile(cli.path)
			if err != nil {
				t.Fatal(err)
			}
			probe := `    envfile=""
    prev=""
    for arg do
      if [ "$prev" = --env-file ]; then envfile="$arg"; fi
      prev="$arg"
    done
    [ -n "$envfile" ] || exit 81
    [ "$(stat -c %a "$envfile")" = 600 ] || exit 82
    [ "$HTTP_PROXY" = http://host-original.invalid:3128 ] || exit 83
    cat "$envfile" > "$PROXY_ENV_EVIDENCE"
`
			script := strings.Replace(string(raw), "  run)\n", "  run)\n"+probe, 1)
			if err := os.WriteFile(cli.path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			const endpoint = "http://t:synthetic-proxy-secret-2923@host.docker.internal:18080"
			spec := lifecycleSpec()
			spec.Env["HTTPS_PROXY"] = endpoint
			d := fakeDriver(t, cli)
			prepared, err := d.Prepare(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "proxy-file", WorkspacePath: t.TempDir(), ProxyEndpoint: endpoint})
			if mode == "always" {
				if err == nil {
					t.Fatal("conflict did not fail")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = run.Cleanup(context.Background()) })
			}
			contents, readErr := os.ReadFile(evidence)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, name := range proxyEnvNames {
				if !strings.Contains(string(contents), name+"="+endpoint+"\n") {
					t.Errorf("missing effective proxy %s", name)
				}
			}
			runCount := 0
			for _, inv := range invocations(t, log) {
				for i, a := range inv {
					if strings.Contains(a, "synthetic-proxy-secret-2923") {
						t.Fatalf("proxy credential on argv: %q", inv)
					}
					if a == "--env-file" {
						if _, e := os.Stat(inv[i+1]); !os.IsNotExist(e) {
							t.Fatalf("private file survives Start: %v", e)
						}
					}
				}
				if inv[0] == "run" {
					runCount++
				}
			}
			want := 1
			if mode != "" {
				want = 2
			}
			if runCount != want {
				t.Fatalf("create attempts %d, want%d", runCount, want)
			}
		})
	}
}

func TestProxyStartEnvFileRefusesLineInjection(t *testing.T) {
	for _, suffix := range []string{"\nINJECTED=yes", "\r", "\x00"} {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		file, err := proxyStartEnvFile("http://synthetic-token@proxy" + suffix)
		if err == nil || file != "" {
			t.Fatalf("invalid endpoint accepted: path%q err%v", file, err)
		}
		entries, e := os.ReadDir(dir)
		if e != nil || len(entries) != 0 {
			t.Fatalf("refusal left files: %v %v", entries, e)
		}
		if strings.Contains(err.Error(), "synthetic-token") {
			t.Fatal("refusal leaked token")
		}
	}
}
