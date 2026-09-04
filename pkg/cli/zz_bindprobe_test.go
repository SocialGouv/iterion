package cli

import "testing"

func TestBindGuardProbe(t *testing.T) {
    err := RunStudio(t.Context(), StudioOptions{Bind: "0.0.0.0", Port: -1, NoBrowser: true, Dir: t.TempDir()}, NewPrinter(OutputHuman))
    t.Logf("err = %v", err)
    if err == nil {
        t.Fatal("expected refusal")
    }
}
