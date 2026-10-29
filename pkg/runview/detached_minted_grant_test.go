package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mintedGrantSentinel stands in for the bearer a server mints at launch: a
// value no real grant can take, so finding it anywhere proves a leak.
const mintedGrantSentinel = "sentinel-detached-grant-2033"

// The detached runner's command line is readable by any co-resident process
// through /proc/<pid>/cmdline for the whole life of the run, so the launch
// grant rides the child's ENVIRONMENT instead — while the endpoints it is
// presented to, which are not credentials, keep riding the argv.
func TestBuildRunnerCmdKeepsTheMintedGrantOutOfTheArgv(t *testing.T) {
	cmd, err := buildRunnerCmd(context.Background(), "/bin/true", detachedSpec{
		Command: runnerCommandRun, RunID: "run-1", FilePath: "wf.bot",
		Vars: map[string]string{
			store.ForgePublishTokenVar: mintedGrantSentinel,
			store.ForgePublishURLVar:   "https://iterion.example/publish",
			"reviewer":                 "alice",
		},
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, mintedGrantSentinel) {
		t.Errorf("the minted grant is in the runner's argv: %q", joined)
	}
	if strings.Contains(joined, "--var "+store.ForgePublishTokenVar+"=") {
		t.Errorf("the runner's argv still names the grant var: %q", joined)
	}
	// Not a silent capability loss: the endpoints and the caller's own vars
	// still reach the child, and the grant reaches it by environment.
	for _, want := range []string{"--var " + store.ForgePublishURLVar + "=https://iterion.example/publish", "--var reviewer=alice"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %q, want %q", joined, want)
		}
	}
	wantEnv := store.ServerMintedSecretEnvName(store.ForgePublishTokenVar) + "=" + mintedGrantSentinel
	found := false
	for _, e := range cmd.Env {
		found = found || e == wantEnv
	}
	if !found {
		t.Errorf("the child's environment does not carry the grant (%d entries), so the detached run cannot publish", len(cmd.Env))
	}
}

// A withdrawal is the caller's explicit "this run does not publish": it is
// not a credential, so it stays on the argv the child reads its var set from,
// and nothing is put in the environment to contradict it.
func TestBuildRunnerCmdCarriesAWithdrawalOnTheArgv(t *testing.T) {
	cmd, err := buildRunnerCmd(context.Background(), "/bin/true", detachedSpec{
		Command: runnerCommandRun, RunID: "run-1", FilePath: "wf.bot",
		Vars: map[string]string{store.ForgePublishTokenVar: ""},
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	if joined := strings.Join(cmd.Args, " "); !strings.Contains(joined, "--var "+store.ForgePublishTokenVar+"=") {
		t.Errorf("args = %q, want the withdrawal forwarded", joined)
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, store.ServerMintedSecretEnvName(store.ForgePublishTokenVar)+"=") {
			t.Error("a withdrawal was turned into a carriage entry in the child's environment")
		}
	}
}
