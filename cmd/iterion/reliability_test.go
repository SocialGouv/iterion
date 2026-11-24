package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// reliabilityTestCmd is a bare command carrying a context and a discarded
// stderr, which is all cloudReliabilitySource and the report's RunE read
// from the cobra side.
func reliabilityTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	return cmd
}

func withReliabilityReportOpts(t *testing.T) {
	t.Helper()
	saved := reliabilityReportOpts
	t.Cleanup(func() { reliabilityReportOpts = saved })
}

// TestCloudBaselineRefusesAnIncompleteConfiguration is the door of the
// cloud baseline (#1006 slice 1): an operator who asks for the population
// a promotion is decided on must be told the cloud configuration is
// missing, never handed a silent fallback to whatever store the working
// directory happens to resolve to.
func TestCloudBaselineRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Setenv("ITERION_MONGO_URI", "")
	t.Setenv("ITERION_S3_BUCKET", "")
	src, closeSrc, err := cloudReliabilitySource(reliabilityTestCmd(t), "", "")
	if err == nil {
		closeSrc()
		t.Fatalf("an unconfigured cloud store must not open: got source %v", src)
	}
	if !strings.Contains(err.Error(), "ITERION_MONGO_URI") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}

// TestReliabilityReportRefusesTwoStoresAtOnce keeps --cloud and
// --store-dir from being resolved by precedence. They name two different
// run stores, and a baseline taken over the wrong one still prints a
// plausible report — the operator's explicit choice is honoured by
// refusing the ambiguity, not by picking one.
func TestReliabilityReportRefusesTwoStoresAtOnce(t *testing.T) {
	withReliabilityReportOpts(t)
	reliabilityReportOpts.cloud = true
	reliabilityReportOpts.storeDir = t.TempDir()
	err := reliabilityReportCmd.RunE(reliabilityTestCmd(t), nil)
	if err == nil {
		t.Fatal("--cloud with --store-dir must be refused, not silently resolved")
	}
	if !strings.Contains(err.Error(), "--store-dir") {
		t.Fatalf("the refusal does not name the conflict: %v", err)
	}
}

// TestReliabilityReportRefusesATenantWithoutCloud refuses a scope the
// local store cannot apply: the filesystem store is not tenant
// partitioned, so a --tenant baseline taken there would count every run
// while claiming to count one tenant's.
func TestReliabilityReportRefusesATenantWithoutCloud(t *testing.T) {
	withReliabilityReportOpts(t)
	reliabilityReportOpts.cloud = false
	reliabilityReportOpts.tenantID = "tenant-a"
	err := reliabilityReportCmd.RunE(reliabilityTestCmd(t), nil)
	if err == nil {
		t.Fatal("--tenant without --cloud must be refused, not ignored")
	}
	if !strings.Contains(err.Error(), "--cloud") {
		t.Fatalf("the refusal does not name what is needed: %v", err)
	}
}
