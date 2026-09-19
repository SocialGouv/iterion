package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/repograph"
	"github.com/SocialGouv/iterion/pkg/repomap"
	"github.com/spf13/cobra"
)

var mapOpts struct {
	root  string
	check bool
}

var mapCmd = &cobra.Command{
	Use:   "map",
	Short: "Generate and check the repository's discoverability commons",
	Long: `Map writes the small, committed indexes an agent reads instead of
grepping a tree it has never seen: one row per Go package (with the
interfaces it exposes), one per docs page and ADR (with its status), and
one per bot bundle and skill.

Everything is derived deterministically — Go's own parser, markdown
headings, the bundle manifest loader. No model call, no embedding.

The artifacts are committed under docs/references/ and held to the tree
by a Go test, so a stale index fails the same check that runs the unit
suite:

  iterion map gen             # rewrite the committed maps
  iterion map gen --check     # fail if they drift, write nothing`,
}

var mapGenCmd = &cobra.Command{
	Use:   "gen",
	Short: "Rewrite the committed maps (or, with --check, fail on drift)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := mapOpts.root
		if root == "" {
			root = "."
		}
		p := newPrinter()
		if mapOpts.check {
			stale, err := repomap.Stale(root)
			if err != nil {
				return err
			}
			if len(stale) > 0 {
				return fmt.Errorf("stale generated maps: %v — run `task map:gen` and commit", stale)
			}
			p.Line("Maps are fresh.")
			return nil
		}
		written, err := repomap.Write(root)
		if err != nil {
			return err
		}
		if len(written) == 0 {
			p.Line("Maps already up to date.")
			return nil
		}
		for _, rel := range written {
			p.Line("wrote %s", rel)
		}
		return nil
	},
}

var graphOpts struct {
	root  string
	depth int
	limit int
	seeds string
}

func graphRoot() string {
	if graphOpts.root != "" {
		return graphOpts.root
	}
	return "."
}

var mapBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build (or refresh) the repository graph cache",
	Long: `Build reads the tree once — Go packages and the symbols they
declare, the calls between them, the links between docs pages, and every
.bot's compiled node graph — and caches the result under .iterion/map/.

The cache is keyed on a fingerprint of the tree; any change rebuilds all
of it. A graph patched in place would be faster and could describe a
repository that no longer exists, which is the one failure an index must
not have.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, rebuilt, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		nodes, edges := g.Stats()
		if p.Format == cli.OutputJSON {
			p.JSON(struct {
				Rebuilt bool                       `json:"rebuilt"`
				Nodes   map[repograph.Kind]int     `json:"nodes"`
				Edges   map[repograph.Rel]int      `json:"edges"`
				Total   struct{ Nodes, Edges int } `json:"total"`
			}{rebuilt, nodes, edges, struct{ Nodes, Edges int }{len(g.Nodes), len(g.Edges)}})
			return nil
		}
		if rebuilt {
			p.Line("Built %d nodes, %d edges.", len(g.Nodes), len(g.Edges))
		} else {
			p.Line("Cache is current: %d nodes, %d edges.", len(g.Nodes), len(g.Edges))
		}
		for _, k := range []repograph.Kind{
			repograph.KindPackage, repograph.KindFile, repograph.KindSymbol,
			repograph.KindDoc, repograph.KindBot, repograph.KindSkill, repograph.KindDSLNode,
		} {
			p.Line("  %-9s %d", k, nodes[k])
		}
		for _, r := range []repograph.Rel{
			repograph.RelImports, repograph.RelContains, repograph.RelDeclares,
			repograph.RelCalls, repograph.RelReferences, repograph.RelLinks,
			repograph.RelFlows, repograph.RelUses,
		} {
			p.Line("  %-9s %d", r, edges[r])
		}
		return nil
	},
}

var mapFindCmd = &cobra.Command{
	Use:   "find <query>",
	Short: "Find nodes by name, and show what they touch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, _, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		hits := g.Find(args[0], graphOpts.limit)
		if p.Format == cli.OutputJSON {
			p.JSON(hits)
			return nil
		}
		if len(hits) == 0 {
			p.Line("No node matches %q.", args[0])
			return nil
		}
		for _, n := range hits {
			p.Line("%s  %s", n.ID, locationOf(n))
			if n.Doc != "" {
				p.Line("    %s", n.Doc)
			}
		}
		return nil
	},
}

var mapNeighboursCmd = &cobra.Command{
	Use:   "neighbours <node-id>",
	Short: "Show everything one edge away from a node",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, _, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		ns := g.Neighbours(args[0])
		if p.Format == cli.OutputJSON {
			p.JSON(ns)
			return nil
		}
		if len(ns) == 0 {
			p.Line("No edge touches %q. Try `iterion map find`.", args[0])
			return nil
		}
		for _, n := range ns {
			arrow := "→"
			if n.Incoming {
				arrow = "←"
			}
			suffix := ""
			if n.Missing {
				suffix = "  (outside the graph)"
			}
			p.Line("%s %-9s %s%s", arrow, n.Rel, n.ID, suffix)
		}
		return nil
	},
}

var mapPathCmd = &cobra.Command{
	Use:   "path <from-id> <to-id>",
	Short: "Shortest directed path between two nodes",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, _, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		hops := g.Path(args[0], args[1])
		if p.Format == cli.OutputJSON {
			p.JSON(hops)
			return nil
		}
		if len(hops) == 0 {
			p.Line("No directed path from %s to %s.", args[0], args[1])
			return nil
		}
		for i, id := range hops {
			p.Line("%2d. %s", i, id)
		}
		return nil
	},
}

var mapImpactCmd = &cobra.Command{
	Use:   "impact <node-id>",
	Short: "What reaches this node — who breaks if it changes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, _, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		nodes := g.Impacted(args[0], graphOpts.depth)
		if graphOpts.limit > 0 && len(nodes) > graphOpts.limit {
			nodes = nodes[:graphOpts.limit]
		}
		if p.Format == cli.OutputJSON {
			p.JSON(nodes)
			return nil
		}
		if len(nodes) == 0 {
			p.Line("Nothing in the graph reaches %s.", args[0])
			return nil
		}
		for _, n := range nodes {
			p.Line("%s  %s", n.ID, locationOf(n))
		}
		return nil
	},
}

var mapRankCmd = &cobra.Command{
	Use:   "rank",
	Short: "Rank the repository around a set of nodes (personalised PageRank)",
	Long: `Rank answers "given that I am working here, what else matters?"
With --seeds it is personalised: the walk restarts on those nodes, so the
ranking favours their neighbourhood. Without seeds it ranks the whole
repository by structural centrality.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p := newPrinter()
		g, _, err := repograph.Load(graphRoot())
		if err != nil {
			return err
		}
		var seeds []string
		for _, s := range strings.Split(graphOpts.seeds, ",") {
			if s = strings.TrimSpace(s); s != "" {
				seeds = append(seeds, s)
			}
		}
		limit := graphOpts.limit
		if limit == 0 {
			limit = 20
		}
		ranked := g.Rank(seeds, limit)
		if p.Format == cli.OutputJSON {
			p.JSON(ranked)
			return nil
		}
		for _, r := range ranked {
			p.Line("%.5f  %s  %s", r.Score, r.Node.ID, locationOf(r.Node))
		}
		return nil
	},
}

// locationOf renders the file a node lives in, which is what a reader
// opens next.
func locationOf(n repograph.Node) string {
	if n.Path == "" {
		return ""
	}
	if n.Line > 0 {
		return fmt.Sprintf("%s:%d", n.Path, n.Line)
	}
	return n.Path
}

func init() {
	f := mapGenCmd.Flags()
	f.StringVar(&mapOpts.root, "root", "", "Repository root (default: the working directory)")
	f.BoolVar(&mapOpts.check, "check", false, "Fail when a committed map drifts from the tree; write nothing")

	for _, c := range []*cobra.Command{mapBuildCmd, mapFindCmd, mapNeighboursCmd, mapPathCmd, mapImpactCmd, mapRankCmd} {
		c.Flags().StringVar(&graphOpts.root, "root", "", "Repository root (default: the working directory)")
	}
	mapFindCmd.Flags().IntVar(&graphOpts.limit, "limit", 20, "Maximum rows")
	mapImpactCmd.Flags().IntVar(&graphOpts.depth, "depth", 2, "How many edges back to walk")
	mapImpactCmd.Flags().IntVar(&graphOpts.limit, "limit", 50, "Maximum rows")
	mapRankCmd.Flags().StringVar(&graphOpts.seeds, "seeds", "", "Comma-separated node ids the ranking restarts on")
	mapRankCmd.Flags().IntVar(&graphOpts.limit, "limit", 20, "Maximum rows")

	mapCmd.AddCommand(mapGenCmd, mapBuildCmd, mapFindCmd, mapNeighboursCmd, mapPathCmd, mapImpactCmd, mapRankCmd)
	rootCmd.AddCommand(mapCmd)
}
