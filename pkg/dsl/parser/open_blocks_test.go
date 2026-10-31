package parser

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain holds every openBlocks answer of the suite to the replay of the
// whole consumed stream (replayOpenBlocks): the kept stack of #2626 and the
// replay it replaces agree at every resync, on every source the suite
// parses, or the reading panics naming the token. And every
// listBlockClosePoint answer to the from-scratch scan (referenceClosePoint):
// the memo of #2768 and the scan it stands for agree at every resync.
func TestMain(m *testing.M) {
	openBlocksCheck = checkOpenBlocks
	closePointCheck = checkClosePoint
	os.Exit(m.Run())
}

func checkOpenBlocks(p *parser, cols []int, hosts []string) {
	wantCols, wantHosts := replayOpenBlocks(p.lex.tokens[:p.lex.ti], p.blocks.skipped)
	if !slices.Equal(cols, wantCols) || !slices.Equal(hosts, wantHosts) {
		panic(fmt.Sprintf("openBlocks at token %d of %s: kept stack %v %q, replay of the consumed stream %v %q", p.lex.ti, p.file, cols, hosts, wantCols, wantHosts))
	}
}

// replayOpenBlocks is the open-block lookup as it stood before #2626 — the
// whole consumed stream walked at every call — kept verbatim as the
// reference the incremental stack is held to, plus the one change of #2771:
// the consumed broken-list remainders (skipped) are not read, since the
// parser's loops never saw them. What the replay read of them was the drift
// #2771 moved.
func replayOpenBlocks(toks []Token, skipped []indexSpan) (cols []int, hosts []string) {
	cols = []int{1}
	hosts = []string{""}
	var lineFirst, lineSecond, lineThird Token
	prevHeader := "" // the keyword of the declaration header the previous line opened
	flush := func() {
		if lineFirst.Type != 0 && declStartsBlock(lineFirst) &&
			tokenAsIdent(lineSecond) != "" && lineThird.Type == TokenColon {
			prevHeader = lineFirst.Value
		} else {
			prevHeader = ""
		}
		lineFirst, lineSecond, lineThird = Token{}, Token{}, Token{}
	}
	si := 0
	for i := 0; i < len(toks); i++ {
		for si < len(skipped) && skipped[si].to <= i {
			si++
		}
		if si < len(skipped) && i >= skipped[si].from {
			i = skipped[si].to - 1
			continue
		}
		switch toks[i].Type {
		case TokenIndent:
			col := 1
			for j := i + 1; j < len(toks); j++ {
				if toks[j].Type != TokenIndent && toks[j].Type != TokenDedent {
					col = toks[j].Column
					break
				}
			}
			cols = append(cols, col)
			hosts = append(hosts, prevHeader)
			lineFirst, lineSecond, lineThird = Token{}, Token{}, Token{}
		case TokenDedent:
			if len(cols) > 1 {
				cols = cols[:len(cols)-1]
				hosts = hosts[:len(hosts)-1]
			}
		case TokenNewline:
			flush()
		default:
			switch {
			case lineFirst.Type == 0:
				lineFirst = toks[i]
			case lineSecond.Type == 0:
				lineSecond = toks[i]
			case lineThird.Type == 0:
				lineThird = toks[i]
			}
		}
	}
	return cols, hosts
}

// withoutOpenBlocksCheck runs f with the suite's cross-check off: the
// reference replay is the quadratic cost whose absence f measures.
func withoutOpenBlocksCheck(t *testing.T, f func()) {
	t.Helper()
	saved := openBlocksCheck
	openBlocksCheck = nil
	defer func() { openBlocksCheck = saved }()
	f()
}

// brokenListsSource is the ticket's shape: n agents, each leaving an inline
// list open at the end of its line, the next header dedented under it —
// 1.7 MB at 40 000.
func brokenListsSource(n int) string {
	var b strings.Builder
	b.Grow(n * 58)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n  tools: [bash,\n", i)
	}
	return b.String()
}

// parseKeeping reads src and hands the parser back, for its stack.
func parseKeeping(name, src string) *parser {
	p := &parser{lex: lexSource(name, src, sourceBudget()), file: name}
	p.parseFile()
	return p
}

// The resync's open-block lookup reads each consumed token once over the
// whole reading, however many broken lists ask: five thousand lists cost
// the stack one pass over the stream. Mutate openBlocks to start the stack
// over at every call — the replay it replaces — and the count goes
// quadratic, past a hundred million on this shape, while every diagnostic
// stays the same.
func TestOpenBlocksReadEachTokenOnce(t *testing.T) {
	withoutOpenBlocksCheck(t, func() {
		for _, c := range []struct {
			name    string
			lists   int
			src     string
			splices bool
		}{
			// The ticket's shape: the rescue bails on every list, splicing
			// nothing.
			{"ticket shape", 5_000, brokenListsSource(5_000), false},
			// One block deeper, each rescue lands on `image:` and splices
			// its DEDENTs — the stack must survive the splice without
			// starting over.
			{"splicing shape", 1_000, deeperBrokenListsSource(1_000), true},
		} {
			p := &parser{lex: lexSource("lists.bot", c.src, sourceBudget()), file: "lists.bot"}
			lexed := p.lex.streamLen()
			p.parseFile()
			if errs := p.report.Count(SeverityError); errs < c.lists {
				t.Fatalf("%s: %d errors for %d broken lists: the shape never reached the resync", c.name, errs, c.lists)
			}
			if p.blocks.replayed == 0 {
				t.Fatalf("%s: the resync never asked openBlocks for the stack", c.name)
			}
			if spliced := p.lex.streamLen() > lexed; spliced != c.splices {
				t.Fatalf("%s: spliced=%v, want %v — the shape no longer exercises what it is here for", c.name, spliced, c.splices)
			}
			if n := p.lex.streamLen(); p.blocks.replayed > n {
				t.Errorf("%s: openBlocks read %d tokens of a stream of %d: a token is read once, however many resyncs ask", c.name, p.blocks.replayed, n)
			}
		}
	})
}

// deeperBrokenListsSource is the ticket's shape one block deeper: the list
// left open sits under `sandbox: network:`, and `image:` follows at the
// sandbox's column, so every rescue lands on an ancestor's property and
// splices the DEDENTs the loops between wait for.
func deeperBrokenListsSource(n int) string {
	var b strings.Builder
	b.Grow(n * 80)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n  sandbox:\n    network:\n      rules: [a,\n    image: i\n", i)
	}
	return b.String()
}

// Under the suite's cross-check, a reading with several resyncs exercises
// the catch-up — the second answer and the later ones read only the tokens
// consumed since — and every answer still equals the replay: the kept stack
// never started over (replayed plus the skipped spans is the cursor of the
// last call, not a sum).
func TestOpenBlocksCatchUpUnderTheCheck(t *testing.T) {
	saved := openBlocksCheck
	calls, lastTi := 0, 0
	openBlocksCheck = func(p *parser, cols []int, hosts []string) {
		calls++
		lastTi = p.lex.ti
		if saved != nil {
			saved(p, cols, hosts)
		}
	}
	defer func() { openBlocksCheck = saved }()
	p := parseKeeping("nested.bot", nestedSource)
	if calls < 2 {
		t.Fatalf("%d openBlocks calls: the source holds three broken lists, the catch-up needs a second call", calls)
	}
	skipped := 0
	for _, sp := range p.blocks.skipped {
		if sp.to <= lastTi {
			skipped += sp.to - sp.from
		}
	}
	if p.blocks.replayed+skipped != lastTi {
		t.Errorf("replayed %d tokens (+%d skipped) over %d calls, the last at cursor %d: the stack started over instead of catching up", p.blocks.replayed, skipped, calls, lastTi)
	}
}

// The replay's drift, moved by #2771: an earlier resync's consumed remainder
// held INDENT/DEDENT pairs the parser's loops never saw, so a second rescue
// in the same block read one level short, landed with one DEDENT too few,
// and handed the declaration after the block to a loop that refused it —
// agent z was lost. The stack now skips the remainders (the loops never saw
// them), the second rescue lands with the two DEDENTs the loops wait for,
// and every declaration after the block reads as itself.
func TestResyncReadsTheStackTheLoopsHold(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  sandbox:\n    network:\n      rules: [a,\n    image: i\n    network:\n      rules: [b,\n  description: \"after\"\nagent z:\n  model: \"m\"\n"
	res := parseLenient("drift.bot", src)
	var at []string
	for _, d := range res.Diagnostics {
		at = append(at, fmt.Sprintf("%s %d:%d", d.Code, d.Line, d.Column))
	}
	want := []string{"E002 5:17", "E002 8:17"}
	if !slices.Equal(at, want) {
		t.Fatalf("diagnostics %v, want the two broken lists' %v", at, want)
	}
	if len(res.File.Agents) != 2 || res.File.Agents[0].Name != "a" || res.File.Agents[0].Description != "after" || res.File.Agents[1].Name != "z" {
		t.Errorf("agents %+v: the drift is fixed — the description stays with agent a and agent z reads as itself", res.File.Agents)
	}
}

// The ticket's measure: a 2.3 MB source of 40 000 agents each leaving a
// list open took 66 s of CPU on the replay, quadratic in the open lists.
// On the kept stack the reading is linear, and held to twenty seconds —
// two orders of magnitude above its cost on a laptop, above the race
// detector's and a loaded runner's slowdowns — so the bound reddens on the
// class, never on the weather.
func TestBrokenListsParseInLinearTime(t *testing.T) {
	withoutOpenBlocksCheck(t, func() {
		const lists = 40_000
		src := brokenListsSource(lists)
		start := time.Now()
		res := parseLenient("lists.bot", src)
		took := time.Since(start)
		if res.ErrorCount < lists {
			t.Fatalf("%d errors for %d broken lists: the shape never reached the resync", res.ErrorCount, lists)
		}
		if bound := 20 * time.Second; took > bound {
			t.Fatalf("%d broken lists (%d bytes) parsed in %s, past the %s bound: the open-block lookup is rescanning the stream", lists, len(src), took, bound)
		}
		t.Logf("%d broken lists (%d bytes) parsed in %s", lists, len(src), took)
	})
}

// The ticket's second quadratic (#2880, the other half of #2768's comment):
// the rescue's splice hands each open loop its closing DEDENTs — inserted
// into the flat token slice, every splice recopied the stream's tail, and a
// source whose broken lists repeat paid the tail per list: measured before
// the queue, N=2000 → 0.33s, 4000 → 1.33s, 8000 → 5.30s, ×4 per doubling,
// the memmove of the tail. The witness is the RATIO, never the clock:
// doubling the lists costs ~2x when the splice is the queue's cut-and-join
// and ~4x when it is a recopy, and neither figure moves with the machine.
// Medians of three interleaved runs keep a loaded shard's pause off the
// verdict.
func TestBrokenListSplicesStayLinear(t *testing.T) {
	withoutOpenBlocksCheck(t, func() {
		parse := func(lists int) time.Duration {
			t.Helper()
			p := &parser{lex: lexSource("lists.bot", deeperBrokenListsSource(lists), sourceBudget()), file: "lists.bot"}
			lexed := p.lex.streamLen()
			start := time.Now()
			p.parseFile()
			took := time.Since(start)
			if errs := p.report.Count(SeverityError); errs < lists {
				t.Fatalf("%d errors for %d broken lists: the shape never reached the resync", errs, lists)
			}
			if p.lex.streamLen() <= lexed {
				t.Fatalf("%d broken lists spliced nothing: the shape no longer exercises the rescue's splice", lists)
			}
			return took
		}
		const lists = 4000
		var small, large []time.Duration
		for r := 0; r < 3; r++ {
			small = append(small, parse(lists))
			large = append(large, parse(2*lists))
		}
		// The ratio reads the BEST run of each size, not the median: a load
		// spike on a shared runner can only inflate a run, so min/min is the
		// reading load cannot fake — and a quadratic rescue scores ≥ 4x even
		// on its best pair (a 3.2x median on a saturated ARC shard failed the
		// 3x bound on 2026-10-31, witness on 63aac5608, flake register).
		s, l := slices.Min(small), slices.Min(large)
		if s <= 0 {
			t.Fatalf("the %d-list parse measured %s: no baseline to compare against", lists, s)
		}
		if ratio := float64(l) / float64(s); ratio > 3 {
			t.Fatalf("doubling the broken lists cost %.1fx (%s then %s): the rescue's splice recopies the stream's tail", ratio, s, l)
		}
		t.Logf("%d broken lists parsed in %s, %d in %s (%.2fx)", lists, s, 2*lists, l, float64(l)/float64(s))
	})
}

// nestedSource is a reading with levels to push and pop: two agents with
// nested blocks, a broken list in each, a stray less-indented line.
const nestedSource = `workflow w:
  entry: a
  budget:
    max_cost_usd: 1
agent a:
  model: "m"
  sandbox:
    network:
      rules: [a,
    image: i
  tools: [bash,
agent b:
  model: "m"
  mcp:
    servers: [x,
  tools: [bash,
`

// The stack answers at any cursor position what the replay of the consumed
// stream answers there — read forward, read back behind what it read (a
// lookahead's rewind starts it over), read at random.
func TestOpenBlocksAgreeWithTheReplayAtEveryCursor(t *testing.T) {
	withoutOpenBlocksCheck(t, func() {
		p := &parser{lex: lexSource("nested.bot", nestedSource, sourceBudget()), file: "nested.bot"}
		n := len(p.lex.tokens)
		if n < 40 {
			t.Fatalf("%d tokens: the source is too small to walk", n)
		}
		positions := make([]int, 0, 3*n)
		for i := 0; i <= n; i++ {
			positions = append(positions, i)
		}
		for i := n; i >= 0; i-- {
			positions = append(positions, i)
		}
		r := rand.New(rand.NewPCG(2626, 1))
		for i := 0; i < n; i++ {
			positions = append(positions, r.IntN(n+1))
		}
		for _, at := range positions {
			p.lex.ti = at
			cols, hosts := p.openBlocks()
			wantCols, wantHosts := replayOpenBlocks(p.lex.tokens[:at], p.blocks.skipped)
			if !slices.Equal(cols, wantCols) || !slices.Equal(hosts, wantHosts) {
				t.Fatalf("at token %d: kept stack %v %q, replay %v %q", at, cols, hosts, wantCols, wantHosts)
			}
		}
	})
}

// A splice behind what the stack read makes the stack start over: the
// DEDENTs spliced in are then in the stream it replays.
func TestOpenBlocksStartOverBehindASplice(t *testing.T) {
	withoutOpenBlocksCheck(t, func() {
		p := &parser{lex: lexSource("nested.bot", nestedSource, sourceBudget()), file: "nested.bot"}
		// Read the stack to the `image:` line, deep in agent a's sandbox.
		at := slices.IndexFunc(p.lex.tokens, func(tok Token) bool { return tok.Value == "image" })
		if at < 0 {
			t.Fatal("no `image` token")
		}
		p.lex.ti = at
		if cols, _ := p.openBlocks(); len(cols) < 3 {
			t.Fatalf("stack %v at `image`: expected the sandbox levels open", cols)
		}
		// Splice two DEDENTs ahead of the `network:` line — behind what the
		// stack read — and read on to `image` again.
		network := slices.IndexFunc(p.lex.tokens, func(tok Token) bool { return tok.Value == "network" })
		p.spliceDedents(network, 2, p.lex.tokens[network].Line)
		p.lex.ti = at + 2
		cols, hosts := p.openBlocks()
		wantCols, wantHosts := replayOpenBlocks(p.lex.tokens[:p.lex.ti], p.blocks.skipped)
		if !slices.Equal(cols, wantCols) || !slices.Equal(hosts, wantHosts) {
			t.Fatalf("after a splice behind the stack: kept stack %v %q, replay %v %q", cols, hosts, wantCols, wantHosts)
		}
	})
}
