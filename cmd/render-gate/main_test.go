package main

import (
	"bytes"
	"strings"
	"testing"
)

const deadValkeyha = "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: iterion-valkeyha\n  namespace: iterion\nspec:\n  template:\n    metadata:\n      labels:\n        app.kubernetes.io/name: valkeyha\n    spec:\n      affinity:\n        podAntiAffinity:\n          requiredDuringSchedulingIgnoredDuringExecution:\n            - topologyKey: kubernetes.io/hostname\n              labelSelector:\n                matchExpressions:\n                  - {key: app.kubernetes.io/name, operator: In, values: [valkey]}\n"

func TestTheCLIRefusesAVerdictOnAnEmptyStream(t *testing.T) {
	// A failed `helm template` upstream of the pipe produces exactly this:
	// nothing. Green here would be a lie.
	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader("# nothing\n"), &stdout, &stderr); code != 1 {
		t.Fatalf("empty stream: exit %d, want 1\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no pod template in the stream") {
		t.Errorf("the refusal must say why: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-allow-empty"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("-allow-empty: exit %d, want 0\n%s", code, stderr.String())
	}
}

func TestTheCLIExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-namespace", "iterion"}, strings.NewReader(deadValkeyha), &stdout, &stderr); code != 1 {
		t.Fatalf("dead selector: exit %d, want 1\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dead: StatefulSet iterion-valkeyha") {
		t.Errorf("the finding must be printed: %s", stdout.String())
	}
	alive := strings.ReplaceAll(deadValkeyha, "values: [valkey]", "values: [valkeyha]")
	stdout.Reset()
	stderr.Reset()
	if code := run(nil, strings.NewReader(alive), &stdout, &stderr); code != 0 {
		t.Fatalf("alive selector: exit %d, want 0\n%s", code, stderr.String())
	}
	if code := run([]string{"-cross", "maybe"}, strings.NewReader(alive), &stdout, &stderr); code != 2 {
		t.Fatalf("bad -cross: exit %d, want 2", code)
	}
	if code := run(nil, strings.NewReader("kind: ConfigMap\nmetadata: [unclosed\n"), &stdout, &stderr); code != 2 {
		t.Fatalf("unreadable stream: exit %d, want 2", code)
	}
}
