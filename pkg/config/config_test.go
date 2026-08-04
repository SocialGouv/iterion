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
	body := "nats:\n  max_ack_pending: 64\n  ack_wait: 5m\n  max_deliver: 3\n  max_age: 12h\n  dlq_max_age: 72h\n  max_payload: 1048576\n"
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
	if cfg.NATS.AckWait != 5*time.Minute || cfg.NATS.MaxAge != 12*time.Hour || cfg.NATS.DLQMaxAge != 72*time.Hour {
		t.Errorf("durations = %s/%s/%s, want 5m/12h/72h", cfg.NATS.AckWait, cfg.NATS.MaxAge, cfg.NATS.DLQMaxAge)
	}
	// Zero-value default preserved: tuning left unset inherits natsq defaults.
	if d := Defaults(); d.NATS.MaxAckPending != 0 || d.NATS.AckWait != 0 {
		t.Errorf("Defaults() tuning fields must stay zero (inherit natsq): %+v", d.NATS)
	}
}
