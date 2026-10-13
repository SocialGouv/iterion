package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(n int) []string {
	var pkgs []string
	for i := 0; i < n; i++ {
		pkgs = append(pkgs, fmt.Sprintf("example.com/m/pkg%02d", i))
	}
	return pkgs
}

// The partition a merge-queue entry relies on: three matrix legs of one commit
// must agree even though each runs `go list` in its own pod.
func TestShardByHashIsDeterministicAcrossInputOrder(t *testing.T) {
	pkgs := fixture(60)
	want := shardByHash(pkgs, 3)

	shuffled := append([]string(nil), pkgs...)
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
	got := shardByHash(shuffled, 3)

	for i := range want {
		if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
			t.Fatalf("shard %d changed with input order:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestShardByHashPartitionsTotalityAndDisjointness(t *testing.T) {
	pkgs := fixture(97)
	shards := shardByHash(pkgs, 5)
	if len(shards) != 5 {
		t.Fatalf("want 5 shards, got %d", len(shards))
	}
	seen := map[string]int{}
	for i, shard := range shards {
		for _, p := range shard {
			if prev, dup := seen[p]; dup {
				t.Fatalf("package %s in shards %d and %d", p, prev, i)
			}
			seen[p] = i
		}
	}
	if len(seen) != len(pkgs) {
		t.Fatalf("want %d packages placed, got %d", len(pkgs), len(seen))
	}
}

// Pins the hash itself: a change here silently repartitions every shard (all
// legs of a queue entry still agree, but the balance churns for no reason).
// Regenerate by running `go run ./scripts/ci/testshards -all` against a tree
// with these package paths — do not recompute by hand.
func TestShardByHashGolden(t *testing.T) {
	pkgs := []string{
		"github.com/SocialGouv/iterion/bots",
		"github.com/SocialGouv/iterion/cmd/iterion",
		"github.com/SocialGouv/iterion/e2e",
		"github.com/SocialGouv/iterion/pkg/server",
		"github.com/SocialGouv/iterion/pkg/store",
	}
	got := shardByHash(pkgs, 3)
	want := [][]string{
		{"github.com/SocialGouv/iterion/pkg/store"},
		{"github.com/SocialGouv/iterion/cmd/iterion", "github.com/SocialGouv/iterion/pkg/server"},
		{"github.com/SocialGouv/iterion/bots", "github.com/SocialGouv/iterion/e2e"},
	}
	for i := range want {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("golden shard %d drifted:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestShardByWeightsLandsHeavyPackagesAlone(t *testing.T) {
	pkgs := []string{"a", "b", "c", "d", "e", "f"}
	table := map[string]float64{"a": 500, "b": 300}
	shards := shardByWeights(pkgs, 3, table)

	// a (500) and b (300) are the heaviest; LPT must give each its own shard
	// before anything lighter is placed.
	lone := 0
	for _, shard := range shards {
		if len(shard) == 1 && (shard[0] == "a" || shard[0] == "b") {
			lone++
		}
	}
	if lone != 2 {
		t.Fatalf("want a and b alone on their shards, got %v", shards)
	}
}

func TestShardByWeightsBalancesLoads(t *testing.T) {
	pkgs := fixture(30)
	table := map[string]float64{}
	for i, p := range pkgs {
		table[p] = float64(100 - i)
	}
	shards := shardByWeights(pkgs, 3, table)
	seen := map[string]bool{}
	for _, shard := range shards {
		for _, p := range shard {
			if seen[p] {
				t.Fatalf("package %s placed twice", p)
			}
			seen[p] = true
		}
	}
	if len(seen) != len(pkgs) {
		t.Fatalf("want %d packages placed, got %d", len(pkgs), len(seen))
	}
}

func TestShardByWeightsIsDeterministicAcrossInputOrder(t *testing.T) {
	pkgs := fixture(40)
	table := map[string]float64{"example.com/m/pkg07": 999}
	want := shardByWeights(pkgs, 3, table)

	shuffled := append([]string(nil), pkgs...)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
	got := shardByWeights(shuffled, 3, table)

	for i := range want {
		if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
			t.Fatalf("shard %d changed with input order:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestEmitPrintsOneShardSpaceJoined(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 1, false, fixture(9), nil, &out)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := strings.Fields(out.String())
	want := shardByHash(fixture(9), 3)[1]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shard 1 printed %v, want %v", got, want)
	}
}

func TestEmitRefusesAnEmptyShardLoudly(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 2, false, []string{"only/one/pkg"}, nil, &out)
	if err == nil {
		t.Fatal("want an error for an empty shard, got nil")
	}
	var e *exitError
	if !errors.As(err, &e) || e.code != 1 {
		t.Fatalf("want exitError code 1, got %v", err)
	}
}

func TestEmitOutOfRangeIndexIsUsageError(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 3, false, fixture(9), nil, &out)
	var e *exitError
	if !errors.As(err, &e) || e.code != 2 {
		t.Fatalf("want exitError code 2, got %v", err)
	}
}

func TestLoadWeights(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "weights.tsv")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("happy path with comments and blank lines", func(t *testing.T) {
		table, err := loadWeights(write(t, "# comment\n\na\t120\nb\t3.5\n"))
		if err != nil {
			t.Fatal(err)
		}
		if table["a"] != 120 || table["b"] != 3.5 {
			t.Fatalf("want a=120 b=3.5, got %v", table)
		}
	})

	t.Run("trailing spaces around path and seconds", func(t *testing.T) {
		table, err := loadWeights(write(t, "a \t120\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := table["a"]; !ok {
			t.Fatalf("trailing space on the path silently dropped the row: %v", table)
		}
	})

	t.Run("CRLF line endings", func(t *testing.T) {
		table, err := loadWeights(write(t, "a\t120\r\nb\t1\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(table) != 2 {
			t.Fatalf("CRLF rows lost: %v", table)
		}
	})

	t.Run("NaN Inf zero and negative seconds are refused", func(t *testing.T) {
		for _, bad := range []string{"NaN", "Inf", "+Inf", "0", "-5"} {
			if _, err := loadWeights(write(t, "a\t"+bad+"\n")); err == nil {
				t.Fatalf("seconds %q accepted", bad)
			}
		}
	})

	t.Run("duplicate rows are refused", func(t *testing.T) {
		if _, err := loadWeights(write(t, "a\t120\na\t1\n")); err == nil {
			t.Fatal("duplicate row accepted")
		}
	})

	t.Run("malformed line is a usage error", func(t *testing.T) {
		_, err := loadWeights(write(t, "a 120\n"))
		var e *exitError
		if !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("want exitError code 2, got %v", err)
		}
	})

	t.Run("missing file surfaces the open error", func(t *testing.T) {
		if _, err := loadWeights(filepath.Join(t.TempDir(), "absent.tsv")); err == nil {
			t.Fatal("missing file accepted")
		}
	})
}

func TestEmitSingleShardIsOneNewlineTerminatedLine(t *testing.T) {
	var out bytes.Buffer
	if err := emit(3, 0, false, fixture(9), nil, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("want exactly one newline-terminated line, got %q", got)
	}
	if strings.Contains(got, "#") || strings.Contains(got, "  ") {
		t.Fatalf("package list polluted or double-spaced: %q", got)
	}
}

// One shuffled input per seed: the partition must not depend on the order the
// packages arrived in, whatever the shuffle.
func TestShardPartitionsAreSeedIndependent(t *testing.T) {
	for _, mode := range []string{"hash", "weights"} {
		for seed := int64(1); seed <= 10; seed++ {
			pkgs := fixture(50)
			table := map[string]float64{"example.com/m/pkg13": 800}
			shuffle := func(in []string) []string {
				out := append([]string(nil), in...)
				rng := rand.New(rand.NewSource(seed))
				rng.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
				return out
			}
			var want, got [][]string
			if mode == "hash" {
				want, got = shardByHash(pkgs, 3), shardByHash(shuffle(pkgs), 3)
			} else {
				want, got = shardByWeights(pkgs, 3, table), shardByWeights(shuffle(pkgs), 3, table)
			}
			for i := range want {
				if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
					t.Fatalf("%s seed %d: shard %d changed with input order", mode, seed, i)
				}
			}
		}
	}
}
