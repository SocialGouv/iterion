package server

import (
	"os"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// envOperator reads an operator-grade variable as the process INHERITED it:
// a value a project `.env` planted answers nothing. The server's guard
// postures — origins, CSRF, security headers, the WS origin, the webhook
// forge list, the private-network reaches, the project-trust switches —
// are the operator's own, and the H-10 review measured a project `.env`
// planting `ITERION_REQUIRE_ORIGIN=0` and friends onto every lane: the
// loaders now refuse the grade at the plant, and THIS is the belt at the
// read, for a marker an older binary's loader left in a child (the planted
// set travels to children). A non-grade name reads the live environment,
// exactly as before.
//
// The completeness test (envtrust_belt_test.go) walks this package's raw
// os.Getenv/os.LookupEnv reads and fails the day a grade name is read
// around this helper.
func envOperator(name string) string {
	if envtrust.OperatorGradeRefused(name) {
		return envtrust.Inherited(name)
	}
	return os.Getenv(name)
}
