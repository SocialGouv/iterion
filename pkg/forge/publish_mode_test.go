package forge

import "testing"

// The server reads publish_mode as the exact literal the engine admits for
// the two bots that declare it (`[enum: "", "off"]`) and that they compare:
// "off" withholds, and nothing else does — no folding, no trimming, so no
// spelling reads one way on the server and another in the run.
func TestPublishModeWithholds_ExactLiteral(t *testing.T) {
	if !PublishModeWithholds("off") {
		t.Error(`"off" must withhold`)
	}
	for _, v := range []string{"", "Off", "OFF", " off", "off ", "false", "no", "${X:-off}"} {
		if PublishModeWithholds(v) {
			t.Errorf("%q withholds; only the exact literal \"off\" does", v)
		}
	}
}
