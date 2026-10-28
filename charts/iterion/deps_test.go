package chart_test

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeChartArchive writes <dir>/charts/<file> holding <name>/Chart.yaml with
// the given chart name — the way helm packages a dependency.
func writeChartArchive(t *testing.T, dir, file, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "charts", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	// maintainers first, with its own nested `name:` — the nats chart's
	// Chart.yaml starts that way, and a check reading the first `name:` it
	// meets named the chart "The" and removed it.
	body := "apiVersion: v2\nmaintainers:\n  - email: authors@example.invalid\n    name: The " + name + " Authors\nname: " + name + "\nversion: 0.0.1\n"
	if err := tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// runDepsCheck runs scripts/chart-check-deps.sh on the scratch chart with a
// fake helm on PATH: `dependency list` prints the table in FAKE_HELM_LIST,
// `dependency update` records itself in the marker file and fails.
func runDepsCheck(t *testing.T, root, chart, listing string) ([]byte, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "chart-check-deps.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "helm-update-called")
	fakeHelm := "#!/bin/sh\ncase \"$1 $2\" in\n  \"dependency list\") printf '%s\\n' \"$FAKE_HELM_LIST\" ;;\n  \"dependency update\") echo update > " + marker + "; exit 1 ;;\n  *) echo \"unexpected helm $*\" >&2; exit 3 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "helm"), []byte(fakeHelm), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, chart)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_HELM_LIST="+listing)
	return cmd.CombinedOutput()
}

func archivesLeft(t *testing.T, chart string) string {
	t.Helper()
	remaining, _ := filepath.Glob(filepath.Join(chart, "charts", "*.tgz"))
	var names []string
	for _, r := range remaining {
		names = append(names, filepath.Base(r))
	}
	return strings.Join(names, ",")
}

// TestChartCheckDepsKeepsEveryDeclaredArchive: two declared dependencies,
// their archives plus stale ones — the declared stay (named by the
// Chart.yaml inside each archive, whatever the file is called), the rest go,
// helm is not asked to fetch anything. The first version of this check
// matched a newline-separated list against " name " patterns and deleted
// every declared archive as soon as a second dependency existed.
func TestChartCheckDepsKeepsEveryDeclaredArchive(t *testing.T) {
	root := t.TempDir()
	chart := filepath.Join(root, "chart")
	if err := os.MkdirAll(filepath.Join(chart, "charts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: scratch\nversion: 0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeChartArchive(t, chart, "nats-2.14.6.tgz", "nats")
	writeChartArchive(t, chart, "s3-2fa-1.0.0.tgz", "s3-2fa")
	writeChartArchive(t, chart, "cert-manager-v1.14.0.tgz", "cert-manager")
	writeChartArchive(t, chart, "minio-14.7.16.tgz", "minio")
	writeChartArchive(t, chart, "nats-foo-0.1.0.tgz", "nats-foo")
	if err := os.WriteFile(filepath.Join(chart, "charts", "junk-1.0.0.tgz"), []byte("not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	listing := "NAME\tVERSION\tREPOSITORY\tSTATUS\nnats\t2.14.x\thttps://example.invalid\tok\ns3-2fa\t1.0.0\thttps://example.invalid\tok\ncert-manager\tv1.14.0\thttps://example.invalid\tok\n"
	out, err := runDepsCheck(t, root, chart, listing)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if got := archivesLeft(t, chart); got != "cert-manager-v1.14.0.tgz,nats-2.14.6.tgz,s3-2fa-1.0.0.tgz" {
		t.Fatalf("archives after the check = %s, want the three declared ones and nothing else\n%s", got, out)
	}
	if _, err := os.Stat(filepath.Join(root, "helm-update-called")); err == nil {
		t.Fatalf("helm dependency update was called although every dependency was ok\n%s", out)
	}
	// A dependency helm reports missing or at the wrong version: the fetch
	// runs (here the fake helm, which fails loudly) and the check surfaces
	// that failure instead of swallowing it.
	for _, status := range []string{"missing", "wrong version"} {
		wrong := strings.Replace(listing, "nats\t2.14.x\thttps://example.invalid\tok", "nats\t2.14.x\thttps://example.invalid\t"+status, 1)
		if out, err := runDepsCheck(t, root, chart, wrong); err == nil {
			t.Fatalf("status %q: a failed dependency fetch must fail the check\n%s", status, out)
		}
		if _, err := os.Stat(filepath.Join(root, "helm-update-called")); err != nil {
			t.Fatalf("status %q: helm dependency update was not attempted", status)
		}
		os.Remove(filepath.Join(root, "helm-update-called"))
	}
}
