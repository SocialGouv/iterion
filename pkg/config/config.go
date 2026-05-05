// Package config loads iterion runtime configuration from environment
// variables and an optional YAML file. Precedence: env > yaml > compiled
// defaults. The loader is safe to call from server, runner, or CLI
// entry points; it never reads ITERION_* vars not listed in the schema
// (see plan §E), so existing wiring (ITERION_DEFAULT_BACKEND, etc.) is
// untouched.
package config

import (
	"fmt"
	"time"
)

// Mode selects the persistence/dispatch backend.
type Mode string

const (
	ModeLocal Mode = "local"
	ModeCloud Mode = "cloud"
)

// LogFormat selects between the human-readable console format and
// structured JSON.
type LogFormat string

const (
	LogFormatHuman LogFormat = "human"
	LogFormatJSON  LogFormat = "json"
)

// Config is the parsed, validated runtime configuration.
//
// Field grouping mirrors the YAML sections so a 1:1 yaml.Marshal of a
// loaded Config is a usable config file. Zero values are treated as
// "use the default"; explicit empties (e.g. an env var set to "") win
// over defaults via the per-field defaultedXxx flags carried by the
// loader.
type Config struct {
	Mode Mode `yaml:"mode"`

	NATS    NATSConfig    `yaml:"nats"`
	Mongo   MongoConfig   `yaml:"mongo"`
	S3      S3Config      `yaml:"s3"`
	Runner  RunnerConfig  `yaml:"runner"`
	Server  ServerConfig  `yaml:"server"`
	Metrics MetricsConfig `yaml:"metrics"`
	Log     LogConfig     `yaml:"log"`
}

// NATSConfig holds the NATS JetStream connection + stream/bucket names.
type NATSConfig struct {
	URL       string `yaml:"url"`
	Stream    string `yaml:"stream"`
	KVBucket  string `yaml:"kv_bucket"`
	DLQStream string `yaml:"dlq_stream"`
}

// MongoConfig holds the Mongo connection + DB + events TTL.
type MongoConfig struct {
	URI           string `yaml:"uri"`
	DB            string `yaml:"db"`
	EventsTTLDays int    `yaml:"events_ttl_days"`
}

// S3Config holds the S3/MinIO connection settings.
type S3Config struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	UsePathStyle    bool   `yaml:"use_path_style"`
}

// RunnerConfig holds runner-specific tuning.
type RunnerConfig struct {
	WorkDir     string        `yaml:"workdir"`
	Concurrency int           `yaml:"concurrency"`
	Heartbeat   time.Duration `yaml:"heartbeat"`
	LockTTL     time.Duration `yaml:"lock_ttl"`
}

// ServerConfig holds server-specific settings (HTTP API + healthz port).
type ServerConfig struct {
	HealthzPort int `yaml:"healthz_port"`
}

// MetricsConfig holds the Prometheus metrics endpoint port.
type MetricsConfig struct {
	Port int `yaml:"port"`
}

// LogConfig holds log format + level. format default differs between
// CLI ("human") and server/runner ("json"); the loader does not
// auto-detect — callers pass DefaultLogFormat in LoadOptions.
type LogConfig struct {
	Format LogFormat `yaml:"format"`
	Level  string    `yaml:"level"`
}

// Defaults returns the compiled-in default Config (independent of env/yaml).
// CLI default for log format = human; server/runner override via
// LoadOptions.DefaultLogFormat.
func Defaults() Config {
	return Config{
		Mode: ModeLocal,
		NATS: NATSConfig{
			Stream:    "ITERION_RUNS",
			KVBucket:  "iterion-run-locks",
			DLQStream: "ITERION_RUNS_DLQ",
		},
		Mongo: MongoConfig{
			DB:            "iterion",
			EventsTTLDays: 90,
		},
		S3: S3Config{
			Region:       "us-east-1",
			Bucket:       "iterion-artifacts",
			UsePathStyle: true,
		},
		Runner: RunnerConfig{
			WorkDir:     "/tmp/iterion",
			Concurrency: 1,
			Heartbeat:   20 * time.Second,
			LockTTL:     60 * time.Second,
		},
		Server: ServerConfig{
			HealthzPort: 4891,
		},
		Metrics: MetricsConfig{
			Port: 9090,
		},
		Log: LogConfig{
			Format: LogFormatHuman,
			Level:  "info",
		},
	}
}

// LoadOptions tunes loader behaviour.
type LoadOptions struct {
	// YAMLPath, if non-empty, is read and merged before env vars (env
	// wins). A missing file is an error; a malformed file is an error.
	YAMLPath string
	// DefaultLogFormat overrides the compiled default ("human") to
	// "json" for server/runner entry points. Env still wins.
	DefaultLogFormat LogFormat
}

// Load builds a Config from defaults <- yaml <- env. Validation is
// strict: cloud mode requires NATS+Mongo+S3 to be reachable-by-config,
// not just by env-set; a missing field returns an error before any IO.
func Load(opts LoadOptions) (Config, error) {
	cfg := Defaults()
	if opts.DefaultLogFormat != "" {
		cfg.Log.Format = opts.DefaultLogFormat
	}

	if opts.YAMLPath != "" {
		if err := loadYAML(opts.YAMLPath, &cfg); err != nil {
			return cfg, fmt.Errorf("config: yaml: %w", err)
		}
	}
	if err := loadEnv(&cfg); err != nil {
		return cfg, fmt.Errorf("config: env: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config: validate: %w", err)
	}
	return cfg, nil
}

// Validate enforces invariants (mode-specific required fields, enum
// membership). Returns the first failure as an error suitable for
// surfacing on CLI startup.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeCloud:
	default:
		return fmt.Errorf("ITERION_MODE %q invalid (want local|cloud)", c.Mode)
	}

	if c.Log.Format != LogFormatHuman && c.Log.Format != LogFormatJSON {
		return fmt.Errorf("ITERION_LOG_FORMAT %q invalid (want human|json)", c.Log.Format)
	}
	switch c.Log.Level {
	case "error", "warn", "info", "debug", "trace":
	default:
		return fmt.Errorf("ITERION_LOG_LEVEL %q invalid (want error|warn|info|debug|trace)", c.Log.Level)
	}

	if c.Server.HealthzPort < 1 || c.Server.HealthzPort > 65535 {
		return fmt.Errorf("ITERION_HEALTHZ_PORT %d invalid (want 1-65535)", c.Server.HealthzPort)
	}
	if c.Metrics.Port < 1 || c.Metrics.Port > 65535 {
		return fmt.Errorf("ITERION_METRICS_PORT %d invalid (want 1-65535)", c.Metrics.Port)
	}

	if c.Mode == ModeCloud {
		if c.NATS.URL == "" {
			return fmt.Errorf("ITERION_NATS_URL required when mode=cloud")
		}
		if c.NATS.Stream == "" {
			return fmt.Errorf("ITERION_NATS_STREAM must not be empty")
		}
		if c.NATS.KVBucket == "" {
			return fmt.Errorf("ITERION_NATS_KV_BUCKET must not be empty")
		}
		if c.NATS.DLQStream == "" {
			return fmt.Errorf("ITERION_NATS_DLQ_STREAM must not be empty")
		}
		if c.Mongo.URI == "" {
			return fmt.Errorf("ITERION_MONGO_URI required when mode=cloud")
		}
		if c.Mongo.DB == "" {
			return fmt.Errorf("ITERION_MONGO_DB must not be empty")
		}
		if c.Mongo.EventsTTLDays < 0 {
			return fmt.Errorf("ITERION_MONGO_EVENTS_TTL_DAYS %d invalid (want >= 0)", c.Mongo.EventsTTLDays)
		}
		if c.S3.Endpoint == "" {
			return fmt.Errorf("ITERION_S3_ENDPOINT required when mode=cloud")
		}
		if c.S3.Bucket == "" {
			return fmt.Errorf("ITERION_S3_BUCKET must not be empty")
		}
		// Access key + secret are conditionally required (IRSA can fill
		// them from the pod environment); we don't enforce them here.
	}

	if c.Runner.Concurrency < 1 {
		return fmt.Errorf("ITERION_RUNNER_CONCURRENCY %d invalid (want >= 1)", c.Runner.Concurrency)
	}

	return nil
}
