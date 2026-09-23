package bots

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A platform credential is consumed BY REFERENCE: the bot is handed the PATH
// of a read-only file through $DEPLOY_CREDENTIAL and never opens or prints it.
// That only holds if every bot declaring one declares the SAME shape — a bot
// that spelled it `as: env`, or dropped `optional`, would either leak the
// bytes into a command line or fail the graph of every run installed without
// one. The four are byte-identical today; this pins that, so the fifth cannot
// drift.
func TestDeployCredentialIsDeclaredTheSameEverywhere(t *testing.T) {
	mainBots, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(mainBots) == 0 {
		t.Fatal("no catalog bots found under bots/*/main.bot")
	}

	declaring := map[string]bool{}
	for _, mainBot := range mainBots {
		bot := filepath.Base(filepath.Dir(mainBot))
		pr := parseBotUnit(mainBot)
		if pr.File == nil {
			continue // parse failure is another test's job
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue // compile failure is another test's job
		}
		sec := cr.Workflow.Secrets["deploy_credential"]
		if sec == nil {
			continue
		}
		declaring[bot] = true
		if sec.As != "file" {
			t.Errorf("%s: deploy_credential as=%q, want \"file\" — by reference, never a value on a command line", bot, sec.As)
		}
		if sec.Env != "DEPLOY_CREDENTIAL" {
			t.Errorf("%s: deploy_credential env=%q, want \"DEPLOY_CREDENTIAL\"", bot, sec.Env)
		}
		if !sec.Optional {
			t.Errorf("%s: deploy_credential is not optional — every run installed without one would fail, including dry runs", bot)
		}
	}

	// modernize is named explicitly: it deploys nothing itself, but a lot's
	// exit gate is declared in the TARGET repository's contract, and a
	// contract may require proof against a DEPLOYED application. With no
	// platform identity reachable, such a gate can only refuse — and its
	// refusal says nothing about the lot.
	for _, want := range []string{"modernize", "review-env", "app-dev", "product-docs"} {
		if !declaring[want] {
			t.Errorf("%s declares no deploy_credential — a gate that must reach a deployed application could only refuse", want)
		}
	}
}
