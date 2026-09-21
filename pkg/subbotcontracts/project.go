package subbotcontracts

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ProjectOutput builds the output a parent's `subbot` node receives from a
// child that keeps a contract (#1280): one entry per output port, read from
// the producing node's outputs — the named field for `from: <node>.<field>`,
// the node's whole output for a whole-schema or file port (`from: <node>`,
// which is the data a publishing node wrote to its artifact). The child's
// actual outputs decide the values: the projection is a selection and a
// rename over what the child produced, never a fabrication. A port whose
// producer — or whose field — the child did not produce is left OUT: the
// parent sees an absent key, which is what a skipped producer means (the
// contract's `nullable` marks the ports a finished run may legitimately
// leave out). A child without a contract never crosses this function: the
// runners keep the terminal-node output for it.
func ProjectOutput(contract *ir.PublicContract, nodeOutputs map[string]map[string]any) map[string]any {
	if contract == nil {
		return nil
	}
	out := make(map[string]any, len(contract.Outputs))
	for _, p := range contract.Outputs {
		if p == nil || p.FromNode == "" {
			continue
		}
		produced := nodeOutputs[p.FromNode]
		if produced == nil {
			continue
		}
		if p.FromField != "" {
			v, ok := produced[p.FromField]
			if !ok {
				continue
			}
			out[p.Name] = v
			continue
		}
		out[p.Name] = produced
	}
	return out
}
