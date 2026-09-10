package ir

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Supervisor diagnostic codes (slot C190–C193, a fresh band above the
// current high-water mark).
const (
	DiagUnknownWatchedNode      DiagCode = "C190" // supervisor watches a node id that isn't an agent node
	DiagMalformedSupervisor     DiagCode = "C191" // supervisor decl is malformed (bad cooldown duration)
	DiagDuplicateSupervisor     DiagCode = "C192" // duplicate supervisor name in workflow
	DiagUnknownSupervisorPrompt DiagCode = "C193" // supervisor system: references an undeclared prompt
)

// Supervisor is the normalized IR form of a `supervisor NAME:`
// declaration. The system prompt is carried as a reference name
// (resolved against Workflow.Prompts at spawn time, like agent system
// prompts); Cooldown is the parsed duration (0 = engine default).
type Supervisor struct {
	Name     string
	Watches  []string
	Model    string
	System   string // prompt reference name
	Cooldown time.Duration
	MaxEvals int
	// Monitors are pre-seeded event patterns, kept in the CLI --monitor
	// grammar ("key=val,key=val"); parsed into supervise.Monitor values
	// at spawn time (supervise.ParseMonitorSpecs — the IR cannot import
	// pkg/supervise, so validateSupervisors syntax-checks the same
	// grammar here and the two are kept in sync).
	Monitors []string
}

// compileSupervisors converts every top-level `supervisor NAME:`
// declaration into a normalized Supervisor. Cross-references (watched
// node ids exist, system prompt declared) are validated separately in
// validateSupervisors, which runs after nodes + prompts are compiled.
func (c *compiler) compileSupervisors() []*Supervisor {
	if len(c.file.Supervisors) == 0 {
		return nil
	}
	out := make([]*Supervisor, 0, len(c.file.Supervisors))
	seen := make(map[string]bool, len(c.file.Supervisors))
	for _, decl := range c.file.Supervisors {
		if seen[decl.Name] {
			c.errorf(DiagDuplicateSupervisor,
				"duplicate supervisor name %q: supervisors must be unique within a file", decl.Name)
			continue
		}
		seen[decl.Name] = true

		sup := &Supervisor{
			Name:     decl.Name,
			Watches:  decl.Watches,
			Model:    decl.Model,
			System:   decl.System,
			MaxEvals: decl.MaxEvals,
			Monitors: decl.Monitors,
		}
		if decl.Cooldown != "" {
			d, err := time.ParseDuration(decl.Cooldown)
			if err != nil {
				c.warnf(DiagMalformedSupervisor,
					"supervisor %q: invalid cooldown %q (want a Go duration like \"30s\") — using the default", decl.Name, decl.Cooldown)
			} else {
				sup.Cooldown = d
			}
		}
		out = append(out, sup)
	}
	return out
}

// validateSupervisors checks the cross-references a supervisor depends
// on, after nodes + prompts are compiled. Both are warnings (not hard
// errors): a supervisor is an enhancement, and a misconfigured one
// should degrade rather than block the whole workflow from compiling.
func (c *compiler) validateSupervisors(w *Workflow) {
	for _, sup := range w.Supervisors {
		for _, nodeID := range sup.Watches {
			n, ok := w.Nodes[nodeID]
			if !ok {
				c.warnf(DiagUnknownWatchedNode,
					"supervisor %q watches %q, which is not a declared node", sup.Name, nodeID)
				continue
			}
			// Agent AND judge nodes are steerable: both execute through the
			// same model executor, whose inbox-drain closure is bound per
			// task regardless of node kind — a judge (e.g. a reviewer) picks
			// up steering at its next turn exactly like an agent.
			if kind := n.NodeKind(); kind != NodeAgent && kind != NodeJudge {
				c.warnf(DiagUnknownWatchedNode,
					"supervisor %q watches %q, which is a %s node — supervisors steer agent and judge nodes", sup.Name, nodeID, kind)
			}
		}
		if sup.System != "" {
			if _, ok := w.Prompts[sup.System]; !ok {
				c.warnf(DiagUnknownSupervisorPrompt,
					"supervisor %q references system prompt %q, which is not declared", sup.Name, sup.System)
			}
		}
		for _, spec := range sup.Monitors {
			if err := CheckMonitorSpec(spec); err != nil {
				c.warnf(DiagMalformedSupervisor,
					"supervisor %q: monitor %q: %v — it will be dropped at spawn", sup.Name, spec, err)
			}
		}
	}
}

// CheckMonitorSpec syntax-checks one pre-seeded monitor spec against the
// grammar pkg/supervise.ParseMonitorSpecs consumes at spawn time
// ("key=val,key=val"; keys: event_type, node_id, tool_name,
// text_contains, cost_gt). Kept in sync with that parser by
// TestMonitorSpecGrammarInSync in the supervise package.
func CheckMonitorSpec(spec string) error {
	fields := 0
	for _, kv := range strings.Split(spec, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("malformed entry %q (want key=val)", kv)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("empty value for key %q", k)
		}
		switch k {
		case "event_type", "node_id", "tool_name", "text_contains":
			// any non-empty string value
		case "cost_gt":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("cost_gt %q is not a number", v)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
				return fmt.Errorf("cost_gt %q must be a finite number > 0", v)
			}
		default:
			return fmt.Errorf("unknown key %q", k)
		}
		fields++
	}
	if fields == 0 {
		return fmt.Errorf("monitor sets no field")
	}
	return nil
}
