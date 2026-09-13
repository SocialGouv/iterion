package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func portIdentity(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e *Engine) nativeRootInputs(inputs map[string]any) (map[string]any, error) {
	if e.workflow.Ports == nil || e.workflow.PublicContract == nil || e.workflow.PortPolicy == nil {
		return nil, fmt.Errorf("runtime: incomplete compiled ports-v1 workflow: %w", store.ErrRunSemantics)
	}
	values := make(map[string]any, len(inputs))
	for name, value := range inputs {
		values[name] = value
	}
	for _, port := range e.workflow.PublicContract.Inputs {
		if _, present := values[port.Name]; !present && len(port.Default) > 0 {
			value, err := ir.DecodePortValue(port.Default)
			if err != nil {
				return nil, err
			}
			values[port.Name] = value
		}
	}
	if err := ir.ValidatePublicValues(e.workflow.PublicContract.Inputs, values); err != nil {
		return nil, fmt.Errorf("runtime: workflow public inputs: %w", err)
	}
	if err := e.workflow.PublicContract.ValidateCriteria("input", values); err != nil {
		return nil, err
	}
	return values, nil
}

func (e *Engine) newPortExecution(ctx context.Context, runID string, inputs map[string]any) (*store.PortExecution, error) {
	bundleHash := ""
	if e.bundle != nil {
		bundleHash = e.bundle.Hash
	}
	source, err := portIdentity(struct {
		Source string
		Bundle string
		IR     *ir.Workflow
	}{e.workflowHash, bundleHash, e.workflow})
	if err != nil {
		return nil, err
	}
	inputIdentity, err := portIdentity(inputs)
	if err != nil {
		return nil, err
	}
	policyIdentity, err := portIdentity(map[string]any{
		"policy": e.workflow.PortPolicy, "budget": e.workflow.Budget,
		"resources": e.workflow.Resources, "members": e.workflow.ResourceMembers,
		"output_corrections": e.outputCorrectionBudget,
	})
	if err != nil {
		return nil, err
	}
	state := &store.PortExecution{
		Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: runID,
		Identity:    store.PortExecutionIdentity{Source: source, Graph: e.workflow.Ports.Identity, Contract: e.workflow.PublicContract.Identity, Policy: policyIdentity, Inputs: inputIdentity},
		Invocations: map[string]*store.PortInvocation{}, Collections: map[string]*store.PortCollection{}, Publications: map[string]*store.PortValue{}, Exports: map[string]string{},
		Products: append([]string(nil), e.workflow.Ports.Products...),
		Budget:   store.PortBudgetState{Reservations: map[string]store.PortBudgetAmount{}},
	}
	for _, port := range e.workflow.PublicContract.Inputs {
		if value, present := inputs[port.Name]; present {
			var files []store.PortFileRef
			if value != nil && port.Type.Name == "file" {
				value, files, err = e.captureRootPortFiles(ctx, runID, port, value)
				if err != nil {
					return nil, fmt.Errorf("runtime: root input %s: %w", port.Name, err)
				}
			}
			if err := addPortValue(state, "input."+port.Name, "input", port.Name, 0, value); err != nil {
				return nil, err
			}
			state.Publications["input."+port.Name].Files = files
		}
	}
	for _, id := range e.workflow.Ports.Order {
		instance := e.workflow.Ports.Nodes[id]
		for _, input := range instance.Contract.Inputs {
			if _, connected := instance.Inputs[input.Name]; !connected && len(input.Default) > 0 {
				value, err := ir.DecodePortValue(input.Default)
				if err != nil {
					return nil, err
				}
				if err := addPortValue(state, "default:"+id+":"+input.Name, "input", input.Name, 0, value); err != nil {
					return nil, err
				}
			}
		}
		if instance.MapInput == "" {
			invocation, err := e.newPortInvocation(instance, id, nil)
			if err != nil {
				return nil, err
			}
			state.Invocations[id] = invocation
		}
	}
	return state, nil
}

func (e *Engine) newPortInvocation(instance *ir.PortInstance, id string, index *int) (*store.PortInvocation, error) {
	implementation, err := portIdentity(struct {
		Node    ir.Node
		Prompts map[string]*ir.Prompt
	}{e.workflow.Nodes[instance.ID], e.workflow.Prompts})
	if err != nil {
		return nil, err
	}
	bindings, err := portIdentity(map[string]any{"inputs": instance.Inputs, "map_input": instance.MapInput, "map_inputs": instance.MapInputs})
	if err != nil {
		return nil, err
	}
	return &store.PortInvocation{ID: id, Node: instance.ID, MapIndex: index, Attempt: 1, Status: store.PortPending,
		Identity: store.PortExecutionIdentity{Source: implementation, Graph: bindings, Contract: instance.Contract.Identity, Policy: instance.Policy.Identity}, Inputs: map[string]string{}}, nil
}

func addPortValue(state *store.PortExecution, revision, producer, port string, attempt int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	publication, err := store.NewPortValue(revision, producer, port, attempt, raw)
	if err != nil {
		return err
	}
	state.Publications[revision] = publication
	return nil
}

func portSource(state *store.PortExecution, endpoint ir.PortEndpoint) (string, bool) {
	if endpoint.Node == ir.WorkflowInputPortNode {
		ref := "input." + endpoint.Port
		if state.Publications[ref] == nil {
			return "", true // root input resolved to absence
		}
		return ref, true
	}
	if invocation := state.Invocations[endpoint.Node]; invocation != nil {
		return invocation.Outputs[endpoint.Port], invocation.Status == store.PortSucceeded
	}
	if collection := state.Collections[endpoint.Node]; collection != nil {
		return collection.Outputs[endpoint.Port], collection.Complete
	}
	return "", false
}

// Connected optional inputs wait for their supplier too. An absent produced
// value never falls through to an unconnected default.
func resolvePortBindings(state *store.PortExecution, instance *ir.PortInstance) (map[string]string, bool) {
	bindings := map[string]string{}
	for _, input := range instance.Contract.Inputs {
		if source, connected := instance.Inputs[input.Name]; connected {
			ref, ready := portSource(state, source)
			if !ready {
				return nil, false
			}
			if ref != "" {
				bindings[input.Name] = ref
			}
		} else if len(input.Default) > 0 {
			bindings[input.Name] = "default:" + instance.ID + ":" + input.Name
		}
	}
	return bindings, true
}

func decodePortInputs(state *store.PortExecution, instance *ir.PortInstance, bindings map[string]string, index *int) (map[string]any, error) {
	values := map[string]any{}
	for name, revision := range bindings {
		publication := state.Publications[revision]
		if publication == nil {
			return nil, fmt.Errorf("runtime: input %s.%s references unpublished revision %s", instance.ID, name, revision)
		}
		value, err := ir.DecodePortValue(publication.Data)
		if err != nil {
			return nil, err
		}
		if index != nil && mappedPortInput(instance, name) {
			items, ok := value.([]any)
			if !ok || *index < 0 || *index >= len(items) {
				return nil, fmt.Errorf("runtime: invalid mapping item for %s.%s", instance.ID, name)
			}
			value = items[*index]
		}
		values[name] = value
	}
	if err := ir.ValidatePublicValues(instance.Contract.Inputs, values); err != nil {
		return nil, fmt.Errorf("runtime: public inputs of %s: %w", instance.ID, err)
	}
	if err := instance.Contract.ValidateCriteria("input", values); err != nil {
		return nil, err
	}
	return values, nil
}

func mappedPortInput(instance *ir.PortInstance, name string) bool {
	for _, input := range instance.MapInputs {
		if name == input {
			return true
		}
	}
	return name == instance.MapInput
}

func portInvocationIDs(state *store.PortExecution, graph *ir.PortGraph) []string {
	var ids []string
	for _, node := range graph.Order {
		if state.Invocations[node] != nil {
			ids = append(ids, node)
		} else if collection := state.Collections[node]; collection != nil {
			ids = append(ids, collection.Items...)
		}
	}
	return ids
}

func portOutputRevision(invocation *store.PortInvocation, name string) string {
	return "output:" + invocation.ID + ":" + strconv.Itoa(invocation.Attempt) + ":" + name
}
