package scripthost

import (
	"strings"
	"testing"
)

// The shape of a real /proc/<pid>/status, with the file-backed pages
// dominating the way they do for the iterion binary: most of VmRSS is RssFile.
const statusWithFilePages = `Name:	iterion
VmPeak:	  812344 kB
VmRSS:	   93500 kB
RssAnon:	    2500 kB
RssFile:	   90000 kB
RssShmem:	    1000 kB
Threads:	9
`

func TestTheProbeChargesAnonAndShmemNotTheFilePages(t *testing.T) {
	got, err := chargedBytes(strings.NewReader(statusWithFilePages))
	if err != nil {
		t.Fatal(err)
	}
	// 2500 + 1000 kB. Reading VmRSS (93500) or adding RssFile is the defect
	// this pins: the binary's pages, paged in as code first runs, killed a
	// legitimate run under the clamp floor in CI.
	if want := int64(3500 * 1024); got != want {
		t.Fatalf("charged %d bytes, want %d (RssAnon + RssShmem, without RssFile)", got, want)
	}
}

func TestAStatusWithoutTheBreakdownIsAnErrorNotAFallback(t *testing.T) {
	for _, tc := range []struct{ name, doc, missing string }{
		{"no RssShmem", "VmRSS:\t 9000 kB\nRssAnon:\t 100 kB\n", "RssShmem"},
		{"no RssAnon", "VmRSS:\t 9000 kB\nRssShmem:\t 100 kB\n", "RssAnon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := chargedBytes(strings.NewReader(tc.doc))
			if err == nil {
				t.Fatalf("a status without %s was read as a number: the probe would compare "+
					"something it never measured", tc.missing)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Errorf("the error does not name the missing field %s: %v", tc.missing, err)
			}
		})
	}
}
