package operatormcp

import (
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
)

type nativeMapProgress struct {
	Node     string `json:"node"`
	Items    int    `json:"items"`
	Complete bool   `json:"complete"`
}

type nativeUsageView struct {
	ReportedTokens  int64   `json:"reported_tokens"`
	ReportedCostUSD float64 `json:"reported_cost_usd"`
	Iterations      int64   `json:"iterations"`
	UnpricedTokens  int64   `json:"unpriced_tokens"`
	UnpricedNodes   int64   `json:"unpriced_nodes"`
	TotalCostKnown  bool    `json:"total_cost_known"`
}

type nativeProgressView struct {
	Invocations map[store.PortInvocationStatus]int `json:"invocations"`
	Maps        []nativeMapProgress                `json:"maps"`
	Products    map[string]string                  `json:"products"`
	Usage       nativeUsageView                    `json:"usage"`
}

// nativeProgress is deliberately a small public status view. Identity
// hashes, input values, file references and recovery internals stay on the
// technical run record, while Copi can still report work, fan-out and cost.
func nativeProgress(state *store.PortExecution) nativeProgressView {
	view := nativeProgressView{
		Invocations: map[store.PortInvocationStatus]int{},
		Maps:        []nativeMapProgress{},
		Products:    map[string]string{},
		Usage: nativeUsageView{
			ReportedTokens:  state.Budget.Consumed.Tokens,
			ReportedCostUSD: state.Budget.Consumed.CostUSD,
			Iterations:      state.Budget.Consumed.Iterations,
			UnpricedTokens:  state.Budget.UnpricedTokens,
			UnpricedNodes:   state.Budget.UnpricedNodes,
			TotalCostKnown:  state.Budget.UnpricedNodes == 0,
		},
	}
	for _, invocation := range state.Invocations {
		if invocation != nil {
			view.Invocations[invocation.Status]++
		}
	}
	for node, collection := range state.Collections {
		if collection != nil {
			view.Maps = append(view.Maps, nativeMapProgress{Node: node, Items: len(collection.Items), Complete: collection.Complete})
		}
	}
	sort.Slice(view.Maps, func(i, j int) bool { return view.Maps[i].Node < view.Maps[j].Node })
	for _, name := range state.Products {
		status := "waiting"
		if revision := state.Exports[name]; revision != "" && state.Publications[revision] != nil {
			status = "published"
		}
		view.Products[name] = status
	}
	return view
}
