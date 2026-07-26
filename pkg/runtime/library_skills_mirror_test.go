package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// Un skill fourni par le bundle ne doit PAS etre signale comme absent de la
// bibliotheque : le miroir de bundle tourne avant celui-ci et le supplante.
func TestAlreadyMirroredCoversBothShapes(t *testing.T) {
	dest := t.TempDir()
	if alreadyMirrored(dest, "absent") {
		t.Fatal("un skill reellement absent ne doit pas passer pour miroite")
	}
	if err := os.MkdirAll(filepath.Join(dest, "dirform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "dirform", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !alreadyMirrored(dest, "dirform") {
		t.Error("forme repertoire <name>/SKILL.md non reconnue")
	}
	if err := os.WriteFile(filepath.Join(dest, "flat.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !alreadyMirrored(dest, "flat") {
		t.Error("forme plate <name>.md non reconnue")
	}
	// Un REPERTOIRE nomme <name>.md ne compte pas.
	if err := os.MkdirAll(filepath.Join(dest, "trap.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if alreadyMirrored(dest, "trap") {
		t.Error("un repertoire ne doit pas compter pour un skill miroite")
	}
}
