// Package team is the public façade over the internal team subsystem
// that backs the team_* and cron_* tools.
package team

import (
	teampkg "github.com/SocialGouv/claw-code-go/internal/runtime/team"
)

type TeamRegistry = teampkg.TeamRegistry
type CronRegistry = teampkg.CronRegistry

func NewTeamRegistry() *TeamRegistry { return teampkg.NewTeamRegistry() }
func NewCronRegistry() *CronRegistry { return teampkg.NewCronRegistry() }
