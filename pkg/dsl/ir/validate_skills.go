package ir

import "regexp"

// skillNameRe enforces the shape of a skill-library reference: a single path
// segment — letters/digits/dash/underscore/dot, not starting with a dot, no
// slashes. It mirrors skilllib.ValidName without importing that package (which
// would pull pkg/store into the pure compiler). Existence in the library is
// deliberately NOT checked here — that is resolved at run time against the
// on-disk library (see runtime.mirrorLibrarySkills), so `iterion validate`
// output stays portable (a bot that references a skill only present on the
// author's machine still compiles cleanly in CI).
var skillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateSkillRefs walks the workflow-level `skills:` default and every
// agent/judge node's `skills:` list, emitting a C199 warning for any malformed
// reference name. It never errors and never rejects an unknown-but-well-formed
// name — the runtime mirror logs a warning when a referenced skill can't be
// resolved in the library, keeping the reference soft (ADR-059).
func (c *compiler) validateSkillRefs(w *Workflow) {
	for _, s := range w.Skills {
		c.validateOneSkillRef(s, "", "workflow")
	}
	for _, n := range w.Nodes {
		ln, ok := n.(LLMNode)
		if !ok {
			continue
		}
		kind := ln.NodeKind().String()
		for _, s := range ln.GetSkills() {
			c.validateOneSkillRef(s, n.NodeID(), kind)
		}
	}
}

func (c *compiler) validateOneSkillRef(name, nodeID, scope string) {
	if name == "" || name == "." || name == ".." || !skillNameRe.MatchString(name) {
		c.warnfAt(DiagInvalidSkillRef, nodeID, "",
			"%s skill reference %q is not a valid skill name (a single path segment: letters, digits, '.', '-', '_', not starting with a dot). It will not be mirrored.",
			scope, name)
	}
}
