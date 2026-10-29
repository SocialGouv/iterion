package secretsink

import "testing"

func TestGrantsDefaultsToEverySinkAndReadsTheListOtherwise(t *testing.T) {
	for _, s := range All {
		if !Known(s) {
			t.Errorf("%s is listed but not known", s)
		}
		if !Grants(nil, s) {
			t.Errorf("an absent list does not grant %s", s)
		}
	}
	if Known("stdout") {
		t.Error("stdout is not a sink")
	}
	grants := []string{Prompt, Egress}
	if !Grants(grants, Prompt) || !Grants(grants, Egress) || Grants(grants, Command) || Grants(grants, Agent) {
		t.Errorf("Grants misreads [prompt, egress]")
	}
}
