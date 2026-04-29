package benchmark

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// RenderReport writes a human-readable text table comparing recipe results.
func RenderReport(w io.Writer, report *BenchmarkReport) {
	fmt.Fprintf(w, "Benchmark: %s\n", report.ID)
	fmt.Fprintf(w, "Case:      %s\n", report.CaseLabel)
	fmt.Fprintf(w, "Date:      %s\n", report.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintln(w, strings.Repeat("─", 80))
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RECIPE\tSTATUS\tVERDICT\tCOST (USD)\tTOKENS\tMODEL CALLS\tITERATIONS\tRETRIES\tDURATION")
	fmt.Fprintln(tw, "──────\t──────\t───────\t──────────\t──────\t───────────\t──────────\t───────\t────────")

	for _, m := range report.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.4f\t%d\t%d\t%d\t%d\t%s\n",
			m.RecipeName,
			m.Status,
			m.Verdict,
			m.TotalCostUSD,
			m.TotalTokens,
			m.ModelCalls,
			m.Iterations,
			m.Retries,
			m.DurationStr,
		)
	}
	tw.Flush()
	fmt.Fprintln(w)
}
