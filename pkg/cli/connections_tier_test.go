package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connection"
)

// TestConnectionsAddNamesTheTierThePackageCameFrom — #2757.
//
// This is the command where a credential is entrusted, and it pins two things
// FROM the package it just resolved: the instance URL it may be sent to and
// the placement it travels in. Two tiers can hold the same connector id at the
// same version with the same scheme id, so the id it echoes says nothing about
// which copy decided — and the operator whose granted project tier shadows the
// package they installed in their home tier has no other surface to see it on.
//
// BOTH DIRECTIONS: the same command against the same connector, once where the
// project tier is the only one that can answer and once where the home tier
// is. A line printing a fixed tier name, or the first tier of the list, is red
// in one arm or the other.
func TestConnectionsAddNamesTheTierThePackageCameFrom(t *testing.T) {
	add := func(t *testing.T, ws string) string {
		t.Helper()
		var out bytes.Buffer
		if err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
			Connector: "forgejo",
			Scheme:    "token",
			BaseURL:   "https://codeberg.org",
			TokenEnv:  "FORGE_TEST_TOKEN",
			StoreDir:  filepath.Join(ws, ".iterion"),
		}, &out); err != nil {
			t.Fatalf("add: %v", err)
		}
		return out.String()
	}
	// The package line, isolated: asserting on the whole output would pass on
	// a tier name printed anywhere, including inside a warning about something
	// else.
	packageLine := func(t *testing.T, out string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "package:") {
				return line
			}
		}
		t.Fatalf("no `package:` line in:\n%s", out)
		return ""
	}

	t.Run("the granted project tier is named", func(t *testing.T) {
		ws := connectionsProjectTierWorkspace(t)
		t.Setenv(connection.ProjectCatalogEnv, "1")
		// The root the command itself reads: its own cwd, which is what
		// `localConnectorTiers` derives the project tier from.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		line := packageLine(t, add(t, ws))
		for _, want := range []string{"forgejo v", string(connection.TierProject) + " catalog", filepath.Join(wd, "connectors")} {
			if !strings.Contains(line, want) {
				t.Errorf("package line = %q, want it to name %q", line, want)
			}
		}
		if strings.Contains(line, string(connection.TierHome)+" catalog") {
			t.Errorf("package line = %q, want no mention of the home tier: it holds nothing here", line)
		}
	})

	// The home tier is the one a default install has, and it is the arm that
	// makes the first one mean something.
	t.Run("the home tier is named", func(t *testing.T) {
		ws := connectionsWorkspace(t)
		line := packageLine(t, add(t, ws))
		for _, want := range []string{"forgejo v", string(connection.TierHome) + " catalog", string(os.PathSeparator) + "connectors"} {
			if !strings.Contains(line, want) {
				t.Errorf("package line = %q, want it to name %q", line, want)
			}
		}
		if strings.Contains(line, string(connection.TierProject)+" catalog") {
			t.Errorf("package line = %q, want no mention of the project tier: it is not granted here", line)
		}
	})
}
