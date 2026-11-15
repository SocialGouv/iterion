package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parkedCommandManifest declares a /parkly command whose mode no lane
// honours yet (deliver, #3136): the manifest parses, the command routes
// nowhere.
const parkedCommandManifest = `name: parked-bot
version: 1.0.0
invocations:
  - kind: command
    mode: deliver
    command:
      name: parkly
      scope: any
`

// Command discovery reads an invocation's mode through bundle.ResolveMode: a
// command whose mode no lane honours resolves to no route — never to a
// route stamped with an empty, launch-by-default mode.
func TestCommandDiscoveryRoutesNoModeNoLaneHonours(t *testing.T) {
	s := newOrgTestServer(t)
	botsDir := t.TempDir()
	for name, manifest := range map[string]string{
		"parked-bot": parkedCommandManifest,
		// The control: the same catalog serves a direct command.
		"direct-bot": strings.NewReplacer("parked-bot", "direct-bot", "deliver", "direct", "parkly", "directly").Replace(parkedCommandManifest),
	} {
		dir := filepath.Join(botsDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(metaBakedBot), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Bots.Paths = []string{botsDir}

	if route, ok := s.cmdDiscoveryFor(context.Background(), "").LookupCommand("directly"); !ok || route.BotID != "direct-bot" {
		t.Fatalf("the control /directly resolved to %+v (ok=%v), want direct-bot", route, ok)
	}
	if route, ok := s.cmdDiscoveryFor(context.Background(), "").LookupCommand("parkly"); ok {
		t.Fatalf("/parkly resolved to %+v, want no route", route)
	}
}
