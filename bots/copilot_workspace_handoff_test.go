package bots

import "testing"

func TestCopilotClosesCrossProjectHandoffWithHostBoundReceipt(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi workspace handoff completion", system,
		`workspace.handoff.complete {status: "completed"|"failed"`,
		"must close that delegated task with exactly one",
		"Never emit this completion action for an ordinary conversation.",
		"Never supply a handoff id, run id, project id, or source path",
		"The reporting action is host-idempotent and does not navigate or switch project focus.",
		"When an action-completed host event names workspace.handoff.complete, do not",
	)
}
