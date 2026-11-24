package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/legal"
	"github.com/SocialGouv/iterion/pkg/secrets"
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
	if cfg.NATS.StreamReplicas != 1 {
		t.Errorf("NATS.StreamReplicas: got %d want 1", cfg.NATS.StreamReplicas)
	}
	if cfg.Mongo.EventsTTLDays != d.Mongo.EventsTTLDays {
		t.Errorf("Mongo.EventsTTLDays: got %d want %d", cfg.Mongo.EventsTTLDays, d.Mongo.EventsTTLDays)
	}
	if cfg.Runner.Concurrency != d.Runner.Concurrency {
		t.Errorf("Runner.Concurrency: got %d want %d", cfg.Runner.Concurrency, d.Runner.Concurrency)
	}
	if cfg.Server.ShutdownDelay != d.Server.ShutdownDelay {
		t.Errorf("Server.ShutdownDelay: got %s want %s", cfg.Server.ShutdownDelay, d.Server.ShutdownDelay)
	}
	if cfg.Log.Format != LogFormatHuman {
		t.Errorf("Log.Format: got %q want %q", cfg.Log.Format, LogFormatHuman)
	}
}

func TestLoad_AlertsDefaults(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.StallTimeout != 5*time.Minute {
		t.Errorf("Alerts.StallTimeout: got %s want 5m", cfg.Alerts.StallTimeout)
	}
	if cfg.Alerts.Webhook.URL != "" {
		t.Errorf("Alerts.Webhook.URL: got %q want empty", cfg.Alerts.Webhook.URL)
	}
	if cfg.Alerts.Desktop.Enabled {
		t.Errorf("Alerts.Desktop.Enabled: got true want false")
	}
}

func TestLoad_AlertsEnvOverride(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_ALERTS_WEBHOOK_URL", "https://hooks.example/abc")
	t.Setenv("ITERION_ALERTS_DESKTOP_ENABLED", "true")
	t.Setenv("ITERION_ALERTS_STALL_TIMEOUT", "90s")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.Webhook.URL != "https://hooks.example/abc" {
		t.Errorf("Webhook.URL = %q", cfg.Alerts.Webhook.URL)
	}
	if !cfg.Alerts.Desktop.Enabled {
		t.Errorf("Desktop.Enabled = false, want true")
	}
	if cfg.Alerts.StallTimeout != 90*time.Second {
		t.Errorf("StallTimeout = %s, want 90s", cfg.Alerts.StallTimeout)
	}
}

func TestLoad_AlertsYAML(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("alerts:\n  webhook:\n    url: https://hooks.example/yaml\n  desktop:\n    enabled: true\n  stall_timeout: 2m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.Webhook.URL != "https://hooks.example/yaml" {
		t.Errorf("Webhook.URL = %q", cfg.Alerts.Webhook.URL)
	}
	if !cfg.Alerts.Desktop.Enabled {
		t.Errorf("Desktop.Enabled = false, want true")
	}
	if cfg.Alerts.StallTimeout != 2*time.Minute {
		t.Errorf("StallTimeout = %s, want 2m", cfg.Alerts.StallTimeout)
	}
}

func TestLoad_SandboxDefaultEnv(t *testing.T) {
	cases := []struct {
		env     string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"none", "none", false},
		{"auto", "auto", false},
		{"AUTO", "auto", false}, // env loader normalises to lowercase
		{"docker", "", true},    // not a Phase 0 mode
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			clearITERION(t)
			if c.env != "" {
				t.Setenv("ITERION_SANDBOX_DEFAULT", c.env)
			}
			cfg, err := Load(LoadOptions{})
			if (err != nil) != c.wantErr {
				t.Fatalf("Load() err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if cfg.Sandbox.Default != c.want {
				t.Errorf("Sandbox.Default = %q, want %q", cfg.Sandbox.Default, c.want)
			}
		})
	}
}

// The server's lame-duck window is what keeps a rolling deploy from
// refusing connections that are still being routed to the terminating
// pod, so it must be on by default — and an operator must be able to
// widen it (slow endpoint propagation) or zero it (fast local restarts).
// The teardown budget that follows it bounds how long in-flight requests
// may take, so it is a knob too, and it may never be zero.
func TestLoad_ServerShutdownBudgets(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ShutdownDelay != 5*time.Second {
		t.Errorf("ShutdownDelay default = %v, want 5s", cfg.Server.ShutdownDelay)
	}
	if cfg.Server.ShutdownTeardown != 30*time.Second {
		t.Errorf("ShutdownTeardown default = %v, want 30s", cfg.Server.ShutdownTeardown)
	}

	for _, c := range []struct {
		key     string
		env     string
		want    time.Duration
		wantErr bool
	}{
		{EnvShutdownDelay, "15s", 15 * time.Second, false},
		{EnvShutdownDelay, "0s", 0, false}, // opting out is legitimate
		{EnvShutdownDelay, "-1s", 0, true},
		{EnvShutdownDelay, "soon", 0, true},
		{EnvShutdownTeardown, "2m", 2 * time.Minute, false},
		// Zero teardown would cut every in-flight request the instant the
		// lame-duck closes — the opposite of a graceful shutdown.
		{EnvShutdownTeardown, "0s", 0, true},
		{EnvShutdownTeardown, "30", 0, true},
	} {
		t.Run(c.key+"="+c.env, func(t *testing.T) {
			clearITERION(t)
			t.Setenv(c.key, c.env)
			cfg, err := Load(LoadOptions{})
			if (err != nil) != c.wantErr {
				t.Fatalf("Load() err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			got := cfg.Server.ShutdownDelay
			if c.key == EnvShutdownTeardown {
				got = cfg.Server.ShutdownTeardown
			}
			if got != c.want {
				t.Errorf("%s = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

// The exported helpers are the SAME parser Load() runs, for surfaces that
// skip the full loader (the local studio). One owner per variable: if
// these drifted from Load(), `iterion studio` and `iterion server` would
// accept different formats for the same env var.
func TestShutdownBudgetsFromEnv(t *testing.T) {
	t.Setenv(EnvShutdownDelay, "")
	if got, err := ShutdownDelayFromEnv(7 * time.Second); err != nil || got != 7*time.Second {
		t.Errorf("unset → (%v, %v), want (7s, nil)", got, err)
	}
	t.Setenv(EnvShutdownDelay, "12s")
	if got, err := ShutdownDelayFromEnv(0); err != nil || got != 12*time.Second {
		t.Errorf("12s → (%v, %v), want (12s, nil)", got, err)
	}
	// A typo must surface, not silently disable the window.
	t.Setenv(EnvShutdownDelay, "5")
	if _, err := ShutdownDelayFromEnv(0); err == nil {
		t.Error(`"5" accepted, want an error naming the unit`)
	}
	t.Setenv(EnvShutdownDelay, "-1s")
	if _, err := ShutdownDelayFromEnv(0); err == nil {
		t.Error("negative delay accepted, want an error")
	}

	t.Setenv(EnvShutdownTeardown, "")
	if got, err := ShutdownTeardownFromEnv(60 * time.Second); err != nil || got != 60*time.Second {
		t.Errorf("unset teardown → (%v, %v), want (60s, nil)", got, err)
	}
	t.Setenv(EnvShutdownTeardown, "90s")
	if got, err := ShutdownTeardownFromEnv(60 * time.Second); err != nil || got != 90*time.Second {
		t.Errorf("90s → (%v, %v), want (90s, nil)", got, err)
	}
	// A zero teardown yields an ALREADY-EXPIRED shutdown context: in-flight
	// runs never flip to failed_resumable and every request is cut. Load()
	// refuses it, so this path must too — otherwise `iterion server --mode
	// local` (which routes to RunStudio and skips Load's Validate) would
	// accept what the same chart rejects in cloud mode.
	t.Setenv(EnvShutdownTeardown, "0s")
	if got, err := ShutdownTeardownFromEnv(60 * time.Second); err == nil {
		t.Errorf("teardown 0s → (%v, nil), want an error (Load() rejects the same value)", got)
	}
	t.Setenv(EnvShutdownTeardown, "-5s")
	if _, err := ShutdownTeardownFromEnv(60 * time.Second); err == nil {
		t.Error("negative teardown accepted, want an error")
	}
}

func TestLoad_RunnerDrainDefaults(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.DrainMode != "complete" {
		t.Errorf("DrainMode default = %q, want complete (lame-duck)", cfg.Runner.DrainMode)
	}
	if cfg.Runner.DrainTimeout != 8*time.Hour {
		t.Errorf("DrainTimeout default = %v, want 8h", cfg.Runner.DrainTimeout)
	}
	if cfg.Runner.SchemaMismatchDelay != 30*time.Second {
		t.Errorf("SchemaMismatchDelay default = %v, want 30s", cfg.Runner.SchemaMismatchDelay)
	}
	if cfg.Rollout.RunnerEpoch != 0 {
		t.Errorf("RunnerEpoch default = %d, want 0", cfg.Rollout.RunnerEpoch)
	}
	if cfg.Rollout.EpochMismatchDelay != 2*time.Minute {
		t.Errorf("EpochMismatchDelay default = %v, want 2m", cfg.Rollout.EpochMismatchDelay)
	}
}

func TestLoad_RolloutEnv(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_RUNNER_EPOCH", "42")
	t.Setenv("ITERION_RUNNER_EPOCH_MISMATCH_DELAY", "3m")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Rollout.RunnerEpoch != 42 || cfg.Rollout.EpochMismatchDelay != 3*time.Minute {
		t.Fatalf("Rollout = %+v, want epoch 42 / delay 3m", cfg.Rollout)
	}

	clearITERION(t)
	t.Setenv("ITERION_RUNNER_EPOCH", "-1")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatal("negative runner epoch accepted")
	}

	clearITERION(t)
	t.Setenv("ITERION_RUNNER_EPOCH_MISMATCH_DELAY", "-1s")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatal("negative epoch mismatch delay accepted")
	}
}

func TestLoad_RunnerSchemaMismatchDelayEnv(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_RUNNER_SCHEMA_MISMATCH_DELAY", "2m")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.SchemaMismatchDelay != 2*time.Minute {
		t.Errorf("SchemaMismatchDelay = %v, want 2m", cfg.Runner.SchemaMismatchDelay)
	}

	clearITERION(t)
	t.Setenv("ITERION_RUNNER_SCHEMA_MISMATCH_DELAY", "-1s")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatal("negative schema mismatch delay accepted, want validation error")
	}
}

func TestLoad_RunnerDrainEnv(t *testing.T) {
	cases := []struct {
		mode    string
		timeout string
		wantErr bool
	}{
		{"complete", "8h", false},
		{"interrupt", "90s", false},
		{"", "2h", false},
		{"paused", "1h", true},  // invalid mode
		{"complete", "", false}, // empty timeout keeps default
	}
	for _, c := range cases {
		t.Run(c.mode+"/"+c.timeout, func(t *testing.T) {
			clearITERION(t)
			if c.mode != "" {
				t.Setenv("ITERION_RUNNER_DRAIN_MODE", c.mode)
			}
			if c.timeout != "" {
				t.Setenv("ITERION_RUNNER_DRAIN_TIMEOUT", c.timeout)
			}
			cfg, err := Load(LoadOptions{})
			if (err != nil) != c.wantErr {
				t.Fatalf("Load() err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if c.mode != "" && cfg.Runner.DrainMode != c.mode {
				t.Errorf("DrainMode = %q, want %q", cfg.Runner.DrainMode, c.mode)
			}
			if c.timeout != "" {
				want, _ := time.ParseDuration(c.timeout)
				if cfg.Runner.DrainTimeout != want {
					t.Errorf("DrainTimeout = %v, want %v", cfg.Runner.DrainTimeout, want)
				}
			}
		})
	}
}

func TestLoad_SandboxOverrideEnv(t *testing.T) {
	cases := []struct {
		env     string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"none", "none", false},
		{"auto", "auto", false},
		{"NONE", "none", false}, // env loader normalises to lowercase
		{"docker", "", true},    // not a mode the override tier accepts
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			clearITERION(t)
			if c.env != "" {
				t.Setenv("ITERION_SANDBOX_OVERRIDE", c.env)
			}
			cfg, err := Load(LoadOptions{})
			if (err != nil) != c.wantErr {
				t.Fatalf("Load() err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if cfg.Sandbox.Override != c.want {
				t.Errorf("Sandbox.Override = %q, want %q", cfg.Sandbox.Override, c.want)
			}
		})
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
auth:
  jwt_secret: "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
  secrets_key: "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
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
  schema_mismatch_delay: "2m"
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
	if cfg.Runner.SchemaMismatchDelay != 2*time.Minute {
		t.Errorf("Runner.SchemaMismatchDelay: got %v want 2m", cfg.Runner.SchemaMismatchDelay)
	}
}

func TestLoad_YAMLRollout(t *testing.T) {
	clearITERION(t)
	path := filepath.Join(t.TempDir(), "rollout.yaml")
	body := `
rollout:
  runner_epoch: 12
  epoch_mismatch_delay: "4m"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Rollout.RunnerEpoch != 12 || cfg.Rollout.EpochMismatchDelay != 4*time.Minute {
		t.Fatalf("Rollout = %+v, want epoch 12 / delay 4m", cfg.Rollout)
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
	t.Setenv("ITERION_JWT_SECRET", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")  // 36 bytes (>32)
	t.Setenv("ITERION_SECRETS_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=") // 36 bytes
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

func TestLoad_SandboxHostStateYAML(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-host-state.yaml")
	body := `
sandbox:
  host_state: none
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sandbox.HostState != "none" {
		t.Errorf("Sandbox.HostState = %q, want none", cfg.Sandbox.HostState)
	}
}

func TestLoad_InvalidRunnerDurations(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"heartbeat-zero", "ITERION_HEARTBEAT_INTERVAL"},
		{"lock-ttl-zero", "ITERION_LOCK_TTL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearITERION(t)
			t.Setenv(tc.env, "0s")
			if _, err := Load(LoadOptions{}); err == nil {
				t.Fatalf("expected error for %s=0s", tc.env)
			}
		})
	}
}

func TestLoad_NATSQueueTuningEnvOverride(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_NATS_STREAM_REPLICAS", "3")
	t.Setenv("ITERION_NATS_MAX_ACK_PENDING", "512")
	t.Setenv("ITERION_NATS_ACK_WAIT", "15m")
	t.Setenv("ITERION_NATS_MAX_DELIVER", "12")
	t.Setenv("ITERION_NATS_MAX_AGE", "48h")
	t.Setenv("ITERION_NATS_DLQ_MAX_AGE", "336h")
	t.Setenv("ITERION_NATS_MAX_PAYLOAD", "4194304")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.MaxAckPending != 512 {
		t.Errorf("MaxAckPending = %d, want 512", cfg.NATS.MaxAckPending)
	}
	if cfg.NATS.StreamReplicas != 3 {
		t.Errorf("StreamReplicas = %d, want 3", cfg.NATS.StreamReplicas)
	}
	if cfg.NATS.AckWait != 15*time.Minute {
		t.Errorf("AckWait = %s, want 15m", cfg.NATS.AckWait)
	}
	if cfg.NATS.MaxDeliver != 12 {
		t.Errorf("MaxDeliver = %d, want 12", cfg.NATS.MaxDeliver)
	}
	if cfg.NATS.MaxAge != 48*time.Hour {
		t.Errorf("MaxAge = %s, want 48h", cfg.NATS.MaxAge)
	}
	if cfg.NATS.DLQMaxAge != 336*time.Hour {
		t.Errorf("DLQMaxAge = %s, want 336h", cfg.NATS.DLQMaxAge)
	}
	if cfg.NATS.MaxPayload != 4194304 {
		t.Errorf("MaxPayload = %d, want 4194304", cfg.NATS.MaxPayload)
	}
}

func TestLoad_NATSQueueTuningYAML(t *testing.T) {
	clearITERION(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "nats:\n  stream_replicas: 2\n  max_ack_pending: 64\n  ack_wait: 5m\n  max_deliver: 3\n  max_age: 12h\n  dlq_max_age: 72h\n  max_payload: 1048576\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.MaxAckPending != 64 || cfg.NATS.MaxDeliver != 3 || cfg.NATS.MaxPayload != 1048576 {
		t.Errorf("ints = %d/%d/%d, want 64/3/1048576", cfg.NATS.MaxAckPending, cfg.NATS.MaxDeliver, cfg.NATS.MaxPayload)
	}
	if cfg.NATS.StreamReplicas != 2 {
		t.Errorf("StreamReplicas = %d, want 2", cfg.NATS.StreamReplicas)
	}
	if cfg.NATS.AckWait != 5*time.Minute || cfg.NATS.MaxAge != 12*time.Hour || cfg.NATS.DLQMaxAge != 72*time.Hour {
		t.Errorf("durations = %s/%s/%s, want 5m/12h/72h", cfg.NATS.AckWait, cfg.NATS.MaxAge, cfg.NATS.DLQMaxAge)
	}
	// Zero-value default preserved: tuning left unset inherits natsq defaults.
	if d := Defaults(); d.NATS.MaxAckPending != 0 || d.NATS.AckWait != 0 {
		t.Errorf("Defaults() tuning fields must stay zero (inherit natsq): %+v", d.NATS)
	}
}

func TestLoad_InvalidNATSStreamReplicas(t *testing.T) {
	for _, value := range []string{"-1", "-3"} {
		t.Run(value, func(t *testing.T) {
			clearITERION(t)
			t.Setenv("ITERION_NATS_STREAM_REPLICAS", value)
			_, err := Load(LoadOptions{})
			if err == nil {
				t.Fatalf("expected error for ITERION_NATS_STREAM_REPLICAS=%s", value)
			}
			if !strings.Contains(err.Error(), "ITERION_NATS_STREAM_REPLICAS") {
				t.Fatalf("error %q does not name ITERION_NATS_STREAM_REPLICAS", err)
			}
		})
	}
}

// Zero is the documented "inherit the default" value for every numeric NATS
// knob, and what NATS itself means by "server default" — rejecting it turned
// a values file into a CrashLoopBackOff on both the server and the runner.
func TestLoad_ZeroNATSStreamReplicasInheritsDefault(t *testing.T) {
	for _, mode := range []string{"local", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			clearITERION(t)
			t.Setenv("ITERION_MODE", mode)
			if mode == "cloud" {
				t.Setenv("ITERION_NATS_URL", "nats://nats:4222")
				t.Setenv("ITERION_MONGO_URI", "mongodb://mongo:27017")
				t.Setenv("ITERION_S3_ENDPOINT", "http://minio:9000")
				t.Setenv("ITERION_JWT_SECRET", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
				t.Setenv("ITERION_SECRETS_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
			}
			t.Setenv("ITERION_NATS_STREAM_REPLICAS", "0")
			cfg, err := Load(LoadOptions{})
			if err != nil {
				t.Fatalf("zero must load (natsq applies the default): %v", err)
			}
			if cfg.NATS.StreamReplicas != 0 {
				t.Fatalf("StreamReplicas = %d, want 0 left for natsq to default", cfg.NATS.StreamReplicas)
			}
		})
	}
}

// The canonical redirect is honoured in every mode, so its validation must be
// too. A local-mode deployment that accepted the flag with no origin would get
// exactly the silently-inert setting the refusal exists to prevent.
func TestValidate_CanonicalRedirectNeedsAnOriginInEveryMode(t *testing.T) {
	cases := []struct {
		name      string
		mode      Mode
		enabled   bool
		publicURL string
		wantErr   bool
	}{
		{"local, on, no origin", ModeLocal, true, "", true},
		{"local, on, scheme-relative", ModeLocal, true, "//iterion.cloud", true},
		{"local, on, path only", ModeLocal, true, "/just/a/path", true},
		{"local, on, valid", ModeLocal, true, "https://iterion.cloud", false},
		{"local, on, trailing slash is still an origin", ModeLocal, true, "https://iterion.cloud/", false},
		// The redirect targets the origin alone, so anything else in the value
		// would be honoured by the OIDC redirect URI and dropped by the
		// redirect — the right host at the wrong path, silently.
		{"local, on, path prefix", ModeLocal, true, "https://iterion.cloud/studio", true},
		{"local, on, query", ModeLocal, true, "https://iterion.cloud?a=b", true},
		{"local, on, fragment", ModeLocal, true, "https://iterion.cloud#f", true},
		{"local, on, userinfo", ModeLocal, true, "https://u:p@iterion.cloud", true},
		{"local, off, path prefix is fine", ModeLocal, false, "https://iterion.cloud/studio", false},
		{"local, off, no origin", ModeLocal, false, "", false},
		{"cloud, on, no origin", ModeCloud, true, "", true},
		{"cloud, on, valid", ModeCloud, true, "https://iterion.cloud", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Mode = tc.mode
			if tc.mode == ModeCloud {
				// Cloud mode refuses on its own prerequisites first, and a
				// probe that mistook THAT refusal for this one would report
				// every case as correctly refused.
				cfg.NATS.URL = "nats://localhost:4222"
				cfg.Mongo.URI = "mongodb://localhost:27017"
				cfg.S3.Endpoint = "http://localhost:9000"
				cfg.Auth.JWTSecret = strings.Repeat("A", 44)
				cfg.Auth.SecretsKey = strings.Repeat("A", 44)
			}
			cfg.Auth.CanonicalRedirect = tc.enabled
			cfg.Auth.PublicURL = tc.publicURL
			// Only the canonical-redirect error may decide this test.
			err := cfg.Validate()
			mentions := err != nil && strings.Contains(err.Error(), "ITERION_CANONICAL_REDIRECT")
			if tc.wantErr && !mentions {
				t.Fatalf("want a canonical-redirect refusal, got %v", err)
			}
			if !tc.wantErr && mentions {
				t.Fatalf("refused a valid pairing: %v", err)
			}
		})
	}
}

// TestLoad_RefreshReuseGrace pins the override of the refresh-token reuse
// grace window: 30s by default, a duration from the env or the YAML, 0 to
// disable it, a negative value refused.
func TestLoad_RefreshReuseGrace(t *testing.T) {
	clearITERION(t)
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.RefreshReuseGrace != 30*time.Second {
		t.Fatalf("default RefreshReuseGrace = %v, want 30s", cfg.Auth.RefreshReuseGrace)
	}

	t.Setenv("ITERION_REFRESH_REUSE_GRACE", "0s")
	if cfg, err = Load(LoadOptions{}); err != nil || cfg.Auth.RefreshReuseGrace != 0 {
		t.Fatalf("ITERION_REFRESH_REUSE_GRACE=0s: got %v (err %v), want 0 (the window disabled)", cfg.Auth.RefreshReuseGrace, err)
	}

	t.Setenv("ITERION_REFRESH_REUSE_GRACE", "")
	path := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  refresh_reuse_grace: \"45s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(LoadOptions{YAMLPath: path}); err != nil || cfg.Auth.RefreshReuseGrace != 45*time.Second {
		t.Fatalf("auth.refresh_reuse_grace: 45s: got %v (err %v)", cfg.Auth.RefreshReuseGrace, err)
	}

	t.Setenv("ITERION_MODE", "cloud")
	t.Setenv("ITERION_NATS_URL", "nats://nats:4222")
	t.Setenv("ITERION_MONGO_URI", "mongodb://mongo:27017")
	t.Setenv("ITERION_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("ITERION_JWT_SECRET", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	t.Setenv("ITERION_SECRETS_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	t.Setenv("ITERION_REFRESH_REUSE_GRACE", "-1s")
	if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "ITERION_REFRESH_REUSE_GRACE") {
		t.Fatalf("a negative ITERION_REFRESH_REUSE_GRACE: got %v, want it refused", err)
	}

	// A window as long as a session's lifetime is refused the same way.
	t.Setenv("ITERION_REFRESH_TTL", "1h")
	for _, grace := range []string{"1h", "2h"} {
		t.Setenv("ITERION_REFRESH_REUSE_GRACE", grace)
		if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "ITERION_REFRESH_REUSE_GRACE") || !strings.Contains(err.Error(), "ITERION_REFRESH_TTL") {
			t.Fatalf("ITERION_REFRESH_REUSE_GRACE=%s on a 1h ITERION_REFRESH_TTL: got %v, want it refused", grace, err)
		}
	}
	t.Setenv("ITERION_REFRESH_REUSE_GRACE", "59m")
	if _, err := Load(LoadOptions{}); err != nil {
		t.Fatalf("ITERION_REFRESH_REUSE_GRACE=59m on a 1h ITERION_REFRESH_TTL: %v", err)
	}
}

// The Anthropic forfait client id is defaulted, so an operator who sets
// nothing keeps the connect flow and the claude_code refresh; an empty env
// var leaves the default, and only an explicit empty YAML value clears it.
func TestLoad_OAuthForfaitAnthropicClientIDDefault(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		env  string
		want string
	}{
		{"nothing set", "", "", secrets.DefaultAnthropicOAuthClientID},
		{"empty env var", "", "set-empty", secrets.DefaultAnthropicOAuthClientID},
		{"YAML section without the key", "auth:\n  oauth_forfait:\n    codex_client_id: codex-x\n", "", secrets.DefaultAnthropicOAuthClientID},
		{"explicit empty YAML value", "auth:\n  oauth_forfait:\n    anthropic_client_id: \"\"\n", "", ""},
		{"explicit YAML override", "auth:\n  oauth_forfait:\n    anthropic_client_id: custom-id\n", "", "custom-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearITERION(t)
			if tc.env == "set-empty" {
				t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID", "")
			}
			opts := LoadOptions{}
			if tc.yaml != "" {
				opts.YAMLPath = filepath.Join(t.TempDir(), "c.yaml")
				if err := os.WriteFile(opts.YAMLPath, []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(opts)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Auth.OAuthForfait.AnthropicClientID; got != tc.want {
				t.Fatalf("AnthropicClientID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_LegalEnv(t *testing.T) {
	clearITERION(t)
	t.Setenv("ITERION_LEGAL_DOCUMENTS_DIR", "/etc/iterion/legal")
	t.Setenv("ITERION_LEGAL_CONTACT_EMAIL", "contact@example.org")
	t.Setenv("ITERION_LEGAL_ACCESSIBILITY_STATUS", "partially_compliant")
	t.Setenv("ITERION_LEGAL_ACCESSIBILITY_DATE", "2026-11-03")
	t.Setenv("ITERION_LEGAL_ACCESSIBILITY_RATE", "72.5")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Legal.DocumentsDir != "/etc/iterion/legal" {
		t.Errorf("DocumentsDir = %q", cfg.Legal.DocumentsDir)
	}
	if cfg.Legal.ContactEmail != "contact@example.org" {
		t.Errorf("ContactEmail = %q", cfg.Legal.ContactEmail)
	}
	a := cfg.Legal.Accessibility()
	if a.Status != legal.StatusPartiallyCompliant || a.Date != "2026-11-03" || a.Rate != "72.5" {
		t.Errorf("Accessibility = %+v", a)
	}
}

func TestLoad_LegalYAML(t *testing.T) {
	clearITERION(t)
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "legal:\n  documents_dir: /srv/legal\n  contact_email: dpo@example.org\n  accessibility_status: non_compliant\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{YAMLPath: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Legal.DocumentsDir != "/srv/legal" || cfg.Legal.ContactEmail != "dpo@example.org" || cfg.Legal.AccessibilityStatus != "non_compliant" {
		t.Errorf("Legal = %+v", cfg.Legal)
	}
}

// The legal surface is validated in every mode: an unknown status, a rate
// with no status, or a contact that is not an address is refused at
// startup rather than published as a half-declaration.
func TestLoad_LegalValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unknown status", map[string]string{"ITERION_LEGAL_ACCESSIBILITY_STATUS": "conforme"}, "ITERION_LEGAL_ACCESSIBILITY_*: accessibility status \"conforme\" invalid"},
		{"rate without status", map[string]string{"ITERION_LEGAL_ACCESSIBILITY_RATE": "42"}, "without an accessibility status"},
		{"bad contact", map[string]string{"ITERION_LEGAL_CONTACT_EMAIL": "not an address"}, "ITERION_LEGAL_CONTACT_EMAIL \"not an address\" invalid"},
		{"display-name contact", map[string]string{"ITERION_LEGAL_CONTACT_EMAIL": "Iterion <contact@iterion.cloud>"}, "without a display name"},
		{"rate contradicts status", map[string]string{"ITERION_LEGAL_ACCESSIBILITY_STATUS": "compliant", "ITERION_LEGAL_ACCESSIBILITY_RATE": "42"}, "contradicts status compliant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearITERION(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, err := Load(LoadOptions{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load = %v, want %q", err, c.want)
			}
		})
	}
}

// The trusted proxy ranges decide whose forwarded header may name the
// client of a request — the audit IP and the auth rate-limit key both
// come from it (#2673). The variable carries them as CSV, and a range
// that parses nowhere is refused where the operator reads the name
// rather than skipped per request, where nothing says so.
func TestLoad_TrustedProxyCIDRs(t *testing.T) {
	t.Run("the CSV reaches the config, trimmed", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_CIDRS", " 10.42.0.0/16 , 2001:db8::/32 ")
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := strings.Join(cfg.Auth.TrustedProxyCIDRs, "|"); got != "10.42.0.0/16|2001:db8::/32" {
			t.Errorf("TrustedProxyCIDRs = %q", got)
		}
	})

	t.Run("unset trusts nothing", func(t *testing.T) {
		clearITERION(t)
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Auth.TrustedProxyCIDRs) != 0 {
			t.Errorf("TrustedProxyCIDRs = %v, want empty: no forwarded header is trusted until the operator names a range", cfg.Auth.TrustedProxyCIDRs)
		}
	})

	// A bare address is the mistake to expect: it looks like a configured
	// proxy and matches nothing.
	t.Run("a bare address is refused, by name", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_CIDRS", "10.42.0.7")
		_, err := Load(LoadOptions{})
		if err == nil {
			t.Fatalf("a bare address must be refused: it would be skipped per request and trust nothing")
		}
		for _, want := range []string{"ITERION_AUTH_TRUSTED_PROXY_CIDRS", "10.42.0.7", "/32"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	// ParseCIDR accepts host bits and hands back the masked network, so
	// this typo would trust a whole subnet in silence — the "never wider"
	// outcome, from the very form the refusal message above invites.
	t.Run("a range with host bits set is refused, with both ways out", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_CIDRS", "10.42.0.7/16")
		_, err := Load(LoadOptions{})
		if err == nil {
			t.Fatalf("10.42.0.7/16 must be refused: it silently trusts all of 10.42.0.0/16")
		}
		for _, want := range []string{"host bits set", "10.42.0.0/16", "10.42.0.7/32"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	// The ranges matter in every mode a proxy can sit in front of, so the
	// refusal is not behind the cloud branch.
	t.Run("a garbled entry is refused in local mode too", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_MODE", "local")
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_CIDRS", "not-a-cidr")
		if _, err := Load(LoadOptions{}); err == nil {
			t.Fatalf("a garbled range must be refused in local mode as well")
		}
	})
}

// The hop count is what tells a proxy's entry in the forwarded chain
// from a client's: the chain is read right to left and stops there. With
// the chart trusting all of private space, an in-range client writes
// entries of its own, so a walk bounded by TRUST instead of by COUNT
// walks into them (#2673).
func TestLoad_TrustedProxyHops(t *testing.T) {
	t.Run("the count reaches the config", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_HOPS", "2")
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.TrustedProxyHops != 2 {
			t.Errorf("TrustedProxyHops = %d, want 2", cfg.Auth.TrustedProxyHops)
		}
	})

	t.Run("unset leaves the server its one-appending-proxy default", func(t *testing.T) {
		clearITERION(t)
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.TrustedProxyHops != 0 {
			t.Errorf("TrustedProxyHops = %d, want 0 — the server reads it as one", cfg.Auth.TrustedProxyHops)
		}
	})

	// A negative count is no walk at all: every request would answer with
	// the proxy's own address and the auth limiter would hold one bucket
	// for the whole instance, which is the outcome the ranges exist to
	// prevent.
	t.Run("a negative count is refused, by name", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_HOPS", "-1")
		_, err := Load(LoadOptions{})
		if err == nil {
			t.Fatalf("-1 must be refused: it reads no hop of the chain and says nothing")
		}
		if !strings.Contains(err.Error(), "ITERION_AUTH_TRUSTED_PROXY_HOPS") {
			t.Errorf("error %q does not name the variable", err)
		}
	})

	t.Run("a value that is not a number is refused", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_TRUSTED_PROXY_HOPS", "two")
		if _, err := Load(LoadOptions{}); err == nil {
			t.Fatalf("a non-numeric count must be refused")
		}
	})
}

// The declared forwarded header is what keeps the CHOICE of mechanism
// away from the client: a server reading whichever of X-Forwarded-For,
// Forwarded or X-Real-IP a request happened to carry would let a client's
// own header beat the one its proxy wrote (#2673). Any field name is
// accepted — a CDN names the client in its own — and a value that is not
// one field name is refused at boot, since a lookup for it would match no
// request and read no client address at all.
func TestLoad_ForwardedHeader(t *testing.T) {
	t.Run("the name reaches the config, trimmed", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_FORWARDED_HEADER", "  X-Real-IP  ")
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.ForwardedHeader != "X-Real-IP" {
			t.Errorf("ForwardedHeader = %q, want X-Real-IP", cfg.Auth.ForwardedHeader)
		}
	})

	t.Run("unset leaves the server its X-Forwarded-For default", func(t *testing.T) {
		clearITERION(t)
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.ForwardedHeader != "" {
			t.Errorf("ForwardedHeader = %q, want empty", cfg.Auth.ForwardedHeader)
		}
	})

	t.Run("a CDN's own header is accepted", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_FORWARDED_HEADER", "CF-Connecting-IP")
		if _, err := Load(LoadOptions{}); err != nil {
			t.Fatalf("a CDN header must be accepted; refusing one would be a bound with no override: %v", err)
		}
	})

	// Surrounding space is an operator's typo the loader forgives, like
	// every other name it reads.
	t.Run("the name is trimmed, not refused, for space around it", func(t *testing.T) {
		clearITERION(t)
		t.Setenv("ITERION_AUTH_FORWARDED_HEADER", "\tx-real-ip\n")
		cfg, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.ForwardedHeader != "x-real-ip" {
			t.Errorf("ForwardedHeader = %q, want x-real-ip", cfg.Auth.ForwardedHeader)
		}
	})

	// A credential-bearing header carries no address, so declaring one
	// reads no client — and the server's diagnosis for that case QUOTES
	// the value it could not parse, which would put a bearer token or a
	// session cookie in the log. Refused at boot, where the operator sees
	// the name they typed.
	for _, bad := range []string{"Authorization", "authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"} {
		t.Run("refused as credential-bearing: "+bad, func(t *testing.T) {
			clearITERION(t)
			t.Setenv("ITERION_AUTH_FORWARDED_HEADER", bad)
			_, err := Load(LoadOptions{})
			if err == nil {
				t.Fatalf("%q must be refused: it names no address, and the server would log the credential it could not parse", bad)
			}
			for _, want := range []string{"ITERION_AUTH_FORWARDED_HEADER", "credential"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}

	// The forms that would read nothing: a list (the declaration is ONE
	// mechanism), a header line, a name with a space inside it.
	for _, bad := range []string{"x-real-ip,x-forwarded-for", "X-Real-IP: 1.2.3.4", "X Real IP"} {
		t.Run("refused: "+bad, func(t *testing.T) {
			clearITERION(t)
			t.Setenv("ITERION_AUTH_FORWARDED_HEADER", bad)
			_, err := Load(LoadOptions{})
			if err == nil {
				t.Fatalf("%q must be refused: a lookup for it matches no request, so no client address is read and nothing says so", bad)
			}
			if !strings.Contains(err.Error(), "ITERION_AUTH_FORWARDED_HEADER") {
				t.Errorf("error %q does not name the variable", err)
			}
		})
	}
}
