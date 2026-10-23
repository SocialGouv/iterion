package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

func TestContracts(t *testing.T) {
	var out bytes.Buffer
	contractsCmd.SetOut(&out)
	t.Cleanup(func() { contractsCmd.SetOut(nil) })
	contractsCmd.Run(contractsCmd, nil)
	var got struct {
		Queue struct {
			Schema int `json:"schema_version"`
			Min    int `json:"min_schema_version"`
		} `json:"queue"`
		Store struct {
			Format int `json:"run_format_version"`
			Mongo  int `json:"mongo_schema_version"`
		} `json:"store"`
		Skew string `json:"build_skew"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Queue.Schema != queue.SchemaVersion || got.Queue.Min != queue.MinSchemaVersion || got.Store.Format != store.RunFormatVersion || got.Store.Mongo != mongostore.SchemaVersion || got.Skew == "" {
		t.Fatalf("contracts: %+v", got)
	}
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	for _, argv := range [][]string{{"iterion", "contracts"}, {"iterion", "--json", "contracts"}} {
		os.Args = argv
		if !isContractsInvocation() {
			t.Fatalf("telemetry not excluded for %v", argv)
		}
	}
	os.Args = []string{"iterion", "server"}
	if isContractsInvocation() {
		t.Fatal("unrelated command excluded from telemetry")
	}
}
