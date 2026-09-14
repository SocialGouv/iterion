package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistributedContractsConfigIsOptionalAndBounded(t *testing.T) {
	if err := (DistributedContractsConfig{}).validate(ModeLocal); err != nil {
		t.Fatalf("default-off local config failed: %v", err)
	}
	good := DistributedContractsConfig{
		SystemNATSURL: "nats://sys:fixture@nats.example:4222", AuthorityRef: "trusted/iterion-authority",
		KubernetesNamespaces: []string{"trusted", "workers"},
	}
	if err := good.validate(ModeCloud); err != nil {
		t.Fatalf("bounded server authority config failed: %v", err)
	}
	for index, mutate := range []func(*DistributedContractsConfig){
		func(d *DistributedContractsConfig) { d.SystemNATSURL = "" },
		func(d *DistributedContractsConfig) { d.SystemNATSURL = "nats://nats.example:4222" },
		func(d *DistributedContractsConfig) { d.SystemNATSURL = "http://sys:fixture@nats.example" },
		func(d *DistributedContractsConfig) { d.AuthorityRef = "outside/iterion-authority" },
		func(d *DistributedContractsConfig) { d.KubernetesNamespaces = []string{"trusted", "trusted"} },
		func(d *DistributedContractsConfig) { d.KubernetesNamespaces = []string{"trusted", ""} },
	} {
		candidate := good
		mutate(&candidate)
		if err := candidate.validate(ModeCloud); err == nil {
			t.Fatalf("incomplete authority configuration case %d was accepted", index)
		} else if strings.Contains(err.Error(), "fixture") {
			t.Fatalf("authority URL credentials leaked in diagnostic: %v", err)
		}
	}
	if err := good.validate(ModeLocal); err == nil {
		t.Fatal("privileged distributed authority accepted in local mode")
	}
}

func TestDistributedContractsYAMLAndEnvironmentOverlay(t *testing.T) {
	clearITERION(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `contracts:
  distributed:
    system_nats_url: nats://yaml:fixture@nats.example:4222
    authority_ref: trusted/yaml-authority
    kubernetes_context: fixture
    kubernetes_namespaces: [trusted, workers]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	if err := loadYAML(path, &cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_CONTRACTS_DISTRIBUTED_SYSTEM_NATS_URL", "nats://env:fixture@nats.example:4222")
	t.Setenv("ITERION_CONTRACTS_DISTRIBUTED_AUTHORITY_REF", "trusted/env-authority")
	t.Setenv("ITERION_CONTRACTS_DISTRIBUTED_KUBERNETES_NAMESPACES", "trusted,workers")
	if err := loadEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	d := cfg.Contracts.Distributed
	if d.SystemNATSURL != "nats://env:fixture@nats.example:4222" || d.AuthorityRef != "trusted/env-authority" ||
		d.KubernetesContext != "fixture" || len(d.KubernetesNamespaces) != 2 {
		t.Fatal("authority configuration precedence failed")
	}
	if err := d.validate(ModeCloud); err != nil {
		t.Fatal(err)
	}
}
