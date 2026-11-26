package main

import (
	"context"
	"fmt"
)

// probeFunc runs the given gate members on the lot's prefix of length k
// (k = 0 is the base) and returns the members that went red.
type probeFunc func(ctx context.Context, k int, members []string) ([]string, error)

// bisect finds the first prefix of the lot on which any of members goes red.
//
// The search needs two endpoints that are already known: prefix 0 (the base)
// green on members, and prefix n red on them — the caller proves both before
// calling, by probing the base and by the rerun of the full lot. Under those
// endpoints it returns k in [1, n] such that prefix k-1 is green and prefix k
// is red: the lot's k-th pull request is the first one whose addition turns a
// member red. That is ⌈log₂ n⌉ probes.
//
// The answer is exact under the one assumption every bisection makes — a red
// member stays red once a prefix carries its cause. A later pull request
// fixing what an earlier one broke breaks that assumption; the caller's full
// regate of the remaining lot after an ejection is what catches it, never this
// function.
func bisect(ctx context.Context, n int, members []string, probe probeFunc) (int, []string, error) {
	if n < 1 {
		return 0, nil, fmt.Errorf("bisect: a lot of %d pull requests has no culprit to find", n)
	}
	if len(members) == 0 {
		return 0, nil, fmt.Errorf("bisect: no member to probe")
	}
	lo, hi := 0, n // lo green, hi red
	culpritRed := members
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		red, err := probe(ctx, mid, members)
		if err != nil {
			return 0, nil, fmt.Errorf("bisect: probe of prefix %d: %w", mid, err)
		}
		if len(red) > 0 {
			hi, culpritRed = mid, red
		} else {
			lo = mid
		}
	}
	return hi, culpritRed, nil
}
