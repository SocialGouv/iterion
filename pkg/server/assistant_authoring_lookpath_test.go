package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The python3 the authoring validation resolves is handed the bot source
// the operator is authoring — which interpreter parses it is an authority
// question, and a PATH a project .env planted must not answer it: the
// refusal reads as the parser being unavailable, never a run of the
// repository's python — #2809 closed the seam, #2876 this site. Red when
// validateAuthoringPythonSyntax returns to exec.LookPath: the fake python3
// then runs under the planted PATH and the refusal case fails on its own
// assertion.
func TestValidateAuthoringPythonSyntaxRefusesAPlantedPATH(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("ITERION_ASSISTANT_PYTHON", "")
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did: the
	// fake interpreter exits 0 and the source parses.
	if err := validateAuthoringPythonSyntax(context.Background(), "main.bot.py", "print(1)\n"); err != nil {
		t.Fatalf("an unplanted PATH must resolve python3: err = %v", err)
	}

	// The repository's does not.
	envtrust.MarkPlanted("PATH")
	err := validateAuthoringPythonSyntax(context.Background(), "main.bot.py", "print(1)\n")
	failure, ok := err.(authoringValidationFailure)
	if !ok || failure.Code != "python_parser_unavailable" {
		t.Errorf("under a planted PATH err = %v, want python_parser_unavailable — the repository does not choose the interpreter", err)
	}
}
