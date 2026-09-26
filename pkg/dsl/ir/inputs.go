package ir

import "sort"

// UnknownInputNames lists the keys of values that name no var of the
// workflow — launch inputs the program could never read, and that every
// launch surface would otherwise drop in silence (#1757): the run would
// execute on defaults while the operator believes it was parameterised.
// Sorted, so an error naming them is stable.
func UnknownInputNames[V any](wf *Workflow, values map[string]V) []string {
	var unknown []string
	for k := range values {
		if _, declared := wf.Vars[k]; !declared {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// DeclaredVarNames lists the workflow's var names, sorted — the set a
// refusal names as the remedy. "none" when the workflow declares none.
func DeclaredVarNames(wf *Workflow) []string {
	names := make([]string, 0, len(wf.Vars))
	for name := range wf.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}
