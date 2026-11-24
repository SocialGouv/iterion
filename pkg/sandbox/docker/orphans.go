package docker

import (
	"context"
	"fmt"
	"strings"
)

// LabelManaged identifies containers spawned by iterion. Mirrors the
// label set in Driver.Start. Used by ReapOrphanContainers to filter
// the runtime's ps listing.
const LabelManaged = "iterion.io/managed=true"

// LabelRunID names the run a managed container belongs to. Format:
// "iterion.io/run-id=<run_id>" (no quoting; run ids are filesystem-safe).
const LabelRunIDPrefix = "iterion.io/run-id="

// OrphanContainer describes a managed container the reaper found.
type OrphanContainer struct {
	ID    string // full SHA returned by `docker ps`
	Name  string // human-readable name (--name from Start)
	RunID string // value of the iterion.io/run-id label
}

// ListManagedContainers returns every container — running or exited —
// labelled iterion.io/managed=true. Used both by the reaper and by
// `iterion sandbox doctor`-style introspection.
func (c CLI) ListManagedContainers(ctx context.Context) ([]OrphanContainer, error) {
	args := []string{
		"ps", "-a",
		"--filter", "label=" + LabelManaged,
		"--format", "{{.ID}}\t{{.Names}}\t{{.Label \"" + strings.TrimSuffix(LabelRunIDPrefix, "=") + "\"}}",
	}
	out, err := c.cmdContext(ctx, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker: list managed containers: %w", err)
	}
	return parseManagedPsOutput(string(out)), nil
}

// parseManagedPsOutput interprets the tab-separated lines emitted by
// `docker ps -a --format "{{.ID}}\t{{.Names}}\t{{.Label ...}}"`.
// Extracted so the parsing is unit-testable without shelling out.
func parseManagedPsOutput(s string) []OrphanContainer {
	var found []OrphanContainer
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		oc := OrphanContainer{ID: parts[0], Name: parts[1]}
		if len(parts) >= 3 {
			oc.RunID = parts[2]
		}
		found = append(found, oc)
	}
	return found
}

// ReapOrphanContainers force-removes every managed container whose run
// the caller's IsTerminal predicate marks as no longer active. Returns
// the IDs that were reaped and the first error encountered (subsequent
// errors are logged via the driver logger when supplied — see
// ReapOrphanContainersWithLogger).
//
// IsTerminal receives the value of the iterion.io/run-id label (empty
// string when the label is missing — treat those as orphans too, since
// a managed container without a run-id has no owner to belong to).
//
// Designed to run at daemon startup, when SIGTERM mid-run could have
// left containers up without the in-process Driver.Run.Cleanup ever
// firing. Safe to call from multiple processes (force-remove of a
// missing container is treated as success).
func (c CLI) ReapOrphanContainers(ctx context.Context, isTerminal func(runID string) bool) ([]string, error) {
	if isTerminal == nil {
		return nil, fmt.Errorf("docker: ReapOrphanContainers: isTerminal predicate is required")
	}
	containers, err := c.ListManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	var reaped []string
	var firstErr error
	for _, oc := range containers {
		if !isTerminal(oc.RunID) {
			continue
		}
		if rmErr := c.forceRemoveContainer(ctx, oc.ID); rmErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("docker: reap %s: %w", containerShortID(oc.ID), rmErr)
			}
			continue
		}
		reaped = append(reaped, oc.ID)
	}
	return reaped, firstErr
}
