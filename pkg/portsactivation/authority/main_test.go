package authority

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

// The production parser deliberately re-executes its own binary with an
// empty environment. Let authority package integration tests exercise that
// same helper protocol through the test executable.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == natsconfig.HelperCommand {
		if err := natsconfig.RunHelper(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
