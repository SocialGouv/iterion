package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearITERION removes any inherited ITERION_* env vars so a Load() call
// observes only the test's overrides.
func clearITERION(t *testing.T) {
	t.Helper()
	for _, e := range os.Environ() {
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			continue
		}
		key := e[:eq]
		if strings.HasPrefix(key, "ITERION_") {
			t.Setenv(key, "")
		}
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := Defaults()
	if cfg.Mode != d.Mode {
		t.Errorf("Mode: got %q want %q", cfg.Mode, d.Mode)
	}
	if cfg.NATS.Stream != d.NATS.Stream {
		t.Errorf("NATS.Stream: got %q want %q", cfg.NATS.Stream, d.NATS.Stream)
	}
	if cfg.Mongo.EventsTTLDays != d.Mongo.EventsTTLDays {
		t.Errorf("Mongo.EventsTTLDays: got %d want %d", cfg.Mongo.EventsTTLDays, d.Mongo.EventsTTLDays)
	}
	if cfg.Runner.Concurrency != d.Runner.Concurrency {
		t.Errorf("Runner.Concurrency: got %d want %d", cfg.Runner.Concurrency, d.Runner.Concurrency)
	}
	if cfg.Server.HealthzPort != d.Server.HealthzPort {
		t.Errorf("HealthzPort: got %d want %d", cfg.Server.HealthzPort, d.Server.HealthzPort)
	}
	if cfg.Log.Format != LogFormatHuman {
		t.Errorf("Log.Format: got %q want %q", cfg.Log.Format, LogFormatHuman)
	}
}

func TestLoad_DefaultLogFormatOverride(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{DefaultLogFormat: LogFormatJSON})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("Log.Format: got %q want %q", cfg.Log.Format, LogFormatJSON)
	}
}

func TestLoad_EnvOverridesYaml(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "iterion.yaml")
	body := `
mode: cloud
nats:
  url: nats://yaml:4222
  stream: YAML_STREAM
mongo:
  uri: mongodb://yaml:27017
s3:
  endpoint: http://yaml:9000
  bucket: yaml-bucket
  access_key_id: yaml-id
  secret_access_key: yaml-secret
runner:
  concurrency: 2
  heartbeat: "10s"
log:
  format: json
  level: debug
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_NATS_URL", "nats://env:4222")
	t.Setenv("ITERION_LOG_LEVEL", "trace")

	cfg, err := Load(LoadOptions{YAMLPath: yamlPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeCloud {
		t.Errorf("Mode: got %q want cloud", cfg.Mode)
	}
	if cfg.NATS.URL != "nats://env:4222" {
		t.Errorf("NATS.URL: got %q want env override", cfg.NATS.URL)
	}
	if cfg.NATS.Stream != "YAML_STREAM" {
		t.Errorf("NATS.Stream: got %q want yaml value", cfg.NATS.Stream)
	}
	if cfg.Log.Level != "trace" {
		t.Errorf("Log.Level: got %q want trace (env override)", cfg.Log.Level)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("Log.Format: got %q want json (yaml)", cfg.Log.Format)
	}
	if cfg.Runner.Heartbeat != 10*time.Second {
		t.Errorf("Runner.Heartbeat: got %v want 10s", cfg.Runner.Heartbeat)
	}
}

func TestLoad_YAMLInvalidRunnerHeartbeat(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-heartbeat.yaml")
	body := `
runner:
  heartbeat: "not-a-duration"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(LoadOptions{YAMLPath: path})
	if err == nil {
		t.Fatalf("expected error on bad yaml runner.heartbeat")
	}
	if !strings.Contains(err.Error(), "runner.heartbeat") {
		t.Fatalf("error %q does not include runner.heartbeat", err)
	}
}

func TestLoad_YAMLInvalidRunnerLockTTL(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-lock-ttl.yaml")
	body := `
runner:
  lock_ttl: "not-a-duration"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(LoadOptions{YAMLPath: path})
	if err == nil {
		t.Fatalf("expected error on bad yaml runner.lock_ttl")
	}
	if !strings.Contains(err.Error(), "runner.lock_ttl") {
		t.Fatalf("error %q does not include runner.lock_ttl", err)
	}
}

func TestLoad_YAMLValidRunnerDurations(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "durations.yaml")
	body := `
runner:
  heartbeat: "7s"
  lock_ttl: "3m"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.Heartbeat != 7*time.Second {
		t.Errorf("Runner.Heartbeat: got %v want 7s", cfg.Runner.Heartbeat)
	}
	if cfg.Runner.LockTTL != 3*time.Minute {
		t.Errorf("Runner.LockTTL: got %v want 3m", cfg.Runner.LockTTL)
	}
}

func TestLoad_ValidationFailsCloudWithoutNATS(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_MODE", "cloud")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error when cloud mode without ITERION_NATS_URL")
	}
}

func TestLoad_ValidationCloudHappyPath(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_MODE", "cloud")
	t.Setenv("ITERION_NATS_URL", "nats://nats:4222")
	t.Setenv("ITERION_MONGO_URI", "mongodb://mongo:27017")
	t.Setenv("ITERION_S3_ENDPOINT", "http://minio:9000")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeCloud {
		t.Errorf("Mode: got %q want cloud", cfg.Mode)
	}
}

func TestLoad_DurationParse(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_HEARTBEAT_INTERVAL", "5s")
	t.Setenv("ITERION_LOCK_TTL", "2m")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.Heartbeat != 5*time.Second {
		t.Errorf("Heartbeat: got %v want 5s", cfg.Runner.Heartbeat)
	}
	if cfg.Runner.LockTTL != 2*time.Minute {
		t.Errorf("LockTTL: got %v want 2m", cfg.Runner.LockTTL)
	}
}

func TestLoad_DurationParseFailure(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_HEARTBEAT_INTERVAL", "not-a-duration")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on bad duration")
	}
}

func TestLoad_BoolParse(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_S3_USE_PATH_STYLE", "false")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.UsePathStyle {
		t.Errorf("UsePathStyle: got true want false (env override)")
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_MODE", "weird")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on invalid mode")
	}
}

func TestLoad_InvalidLogFormat(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_LOG_FORMAT", "xml")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on invalid log format")
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_LOG_LEVEL", "verbose")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on invalid log level")
	}
}

func TestLoad_InvalidConcurrency(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_RUNNER_CONCURRENCY", "0")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on concurrency < 1")
	}
}

func TestLoad_NegativeEventsTTL(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_MODE", "cloud")
	t.Setenv("ITERION_NATS_URL", "nats://nats:4222")
	t.Setenv("ITERION_MONGO_URI", "mongodb://mongo:27017")
	t.Setenv("ITERION_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("ITERION_MONGO_EVENTS_TTL_DAYS", "-1")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatalf("expected error on negative events TTL")
	}
}

func TestLoad_MissingYAMLFile(t *testing.T) {
	clearITERION(t)
	if _, err := Load(LoadOptions{YAMLPath: "/nonexistent/iterion.yaml"}); err == nil {
		t.Fatalf("expected error on missing yaml file")
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("[: not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(LoadOptions{YAMLPath: path}); err == nil {
		t.Fatalf("expected error on malformed yaml")
	}
}

func TestLoad_EmptyEnvIgnored(t *testing.T) {
	// An env var set to "" should not override the default.
	clearITERION(t)
	t.Setenv("ITERION_NATS_STREAM", "")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.Stream != "ITERION_RUNS" {
		t.Errorf("NATS.Stream: got %q want default", cfg.NATS.Stream)
	}
}
