package scripthost

import (
	"strings"
	"testing"
)

// The GUARANTEE, run through the product in hostile conditions.
//
// This replaces an enumeration of JavaScript shapes the guard "must
// recognise" — the shape of test the loop protocol says to abandon once two
// rounds in a row find defects in the previous round's own fixes. That
// happened here: round 2 moved the promise guard from Go into a
// `JSON.stringify` replacer, and round 3 measured four one-line programs that
// disarmed it. So the question stops being "which spellings does the
// predicate catch" and becomes "can a PROGRAM get an unsettled promise past
// this host and have the host call it a success".
//
// Every row is a complete program run through runScript under DefaultLimits,
// the way a node runs one. A row's verdict is `refuse` (the host must not
// report success) or `residual` (a documented gap: the host DOES encode it,
// and the row exists so the day someone closes the gap this table says so).
func TestNoProgramGetsAnUnsettledPromisePastTheHostAsASuccess(t *testing.T) {
	t.Parallel()
	const p = `(async function(){ return 41 + 1 })()`

	rows := []struct {
		name     string
		src      string
		residual bool
	}{
		// The plain shapes.
		{"bare promise", p, false},
		{"pending promise", `new Promise(function(){})`, false},
		{"promise in an object", `({ report: ` + p + ` })`, false},
		{"promise in an array", `[ ` + p + ` ]`, false},
		{"promise nested twice", `({ outer: { inner: ` + p + ` } })`, false},

		// The hostile ones: the script owns JSON.stringify and toJSON, so the
		// DEPTH half is advisory and the ROOT test is what holds. Each of
		// these was measured ENCODING a promise as a success when the root
		// test had been removed in favour of the replacer alone.
		{"JSON.stringify replaced outright", `JSON.stringify = function(v){ return "{}" }; ` + p, false},
		{"JSON.stringify wrapped (the commonplace accident)", `var S = JSON.stringify; JSON.stringify = function(v){ return S(v) }; ` + p, false},
		{"toJSON on the promise instance", `(function(){ var q = ` + p + `; q.toJSON = function(){ return 7 }; return q })()`, false},
		{"toJSON on Promise.prototype", `Promise.prototype.toJSON = function(){ return {done:true} }; ` + p, false},

		// The documented residuals. A promise a program hides where JSON
		// renders nothing: the root is not a promise and the replacer never
		// sees it. The comment in vm.go names these three.
		{"promise inside a Map", `new Map([["k", ` + p + `]])`, true},
		{"promise inside a Set", `new Set([` + p + `])`, true},
		{"promise behind a Proxy", `new Proxy(` + p + `, {})`, true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			lim := DefaultLimits()
			resp := runScript(newRuntime([32]byte{}, epoch), row.src, lim.Wall, lim.ResultBytes)
			if row.residual {
				if !resp.OK() {
					t.Errorf("a documented residual is now REFUSED (%s: %s) — good news, and this row must move out of the residual set and the comment in vm.go with it", resp.Refusal, resp.Message)
				}
				return
			}
			if resp.OK() {
				t.Errorf("the host reported SUCCESS with value %q for a program whose value is an unsettled promise", resp.Value)
			}
			if !strings.Contains(resp.Message, "Promise") {
				t.Errorf("refused as %q: %q — the refusal must name the promise, or a model cannot act on it", resp.Refusal, resp.Message)
			}
		})
	}
}
