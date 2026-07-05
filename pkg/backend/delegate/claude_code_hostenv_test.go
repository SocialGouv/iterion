package delegate

import "testing"

// TestHostSpawnEnv_SeedsOsEnviron guards the fix for the formatOutput
// host-side env strip: the structured-output format pass must inherit the
// ambient environment (PATH/HOME/credential vars), not run with only the
// per-task entries.
func TestHostSpawnEnv_SeedsOsEnviron(t *testing.T) {
	t.Setenv("ITERION_HOSTENV_PROBE", "ambient")
	got := hostSpawnEnv(map[string]string{"EXTRA_KEY": "extra"})

	hasAmbient, hasExtra := false, false
	for _, e := range got {
		switch e {
		case "ITERION_HOSTENV_PROBE=ambient":
			hasAmbient = true
		case "EXTRA_KEY=extra":
			hasExtra = true
		}
	}
	if !hasAmbient {
		t.Fatal("hostSpawnEnv dropped the inherited env (ITERION_HOSTENV_PROBE missing) — the format pass would run with a stripped environment")
	}
	if !hasExtra {
		t.Fatal("hostSpawnEnv did not include the per-task entry EXTRA_KEY")
	}
}

// TestHostSpawnEnv_PerTaskWinsOverInherited verifies per-task entries keep
// precedence over inherited values (os/exec keeps the last duplicate key, so
// the per-task entry must appear after the inherited one).
func TestHostSpawnEnv_PerTaskWinsOverInherited(t *testing.T) {
	t.Setenv("ITERION_HOSTENV_DUP", "inherited")
	got := hostSpawnEnv(map[string]string{"ITERION_HOSTENV_DUP": "override"})

	lastInherited, lastOverride := -1, -1
	for i, e := range got {
		switch e {
		case "ITERION_HOSTENV_DUP=inherited":
			lastInherited = i
		case "ITERION_HOSTENV_DUP=override":
			lastOverride = i
		}
	}
	if lastOverride < 0 {
		t.Fatal("per-task override missing from hostSpawnEnv output")
	}
	if lastInherited >= 0 && lastOverride < lastInherited {
		t.Fatalf("per-task override (idx %d) must come after inherited value (idx %d) so os/exec last-wins picks it", lastOverride, lastInherited)
	}
}
