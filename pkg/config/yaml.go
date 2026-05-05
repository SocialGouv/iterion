package config

import (
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v2"
)

// loadYAML reads path and overlays its contents onto cfg. Unknown keys
// are silently ignored (yaml.v2 default); a malformed file is an error.
// The resulting cfg keeps any defaults for fields the YAML didn't set.
func loadYAML(path string, cfg *Config) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var raw yamlConfig
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := raw.applyTo(cfg); err != nil {
		return err
	}
	return nil
}

// yamlConfig mirrors Config but uses pointer fields so we can detect
// "set to zero" vs "not present in yaml" — without pointers, an absent
// field would zero the default. Only fields explicitly present in the
// YAML overwrite cfg.
type yamlConfig struct {
	Mode    *string            `yaml:"mode"`
	NATS    *yamlNATSConfig    `yaml:"nats"`
	Mongo   *yamlMongoConfig   `yaml:"mongo"`
	S3      *yamlS3Config      `yaml:"s3"`
	Runner  *yamlRunnerConfig  `yaml:"runner"`
	Server  *yamlServerConfig  `yaml:"server"`
	Metrics *yamlMetricsConfig `yaml:"metrics"`
	Log     *yamlLogConfig     `yaml:"log"`
}

type yamlNATSConfig struct {
	URL       *string `yaml:"url"`
	Stream    *string `yaml:"stream"`
	KVBucket  *string `yaml:"kv_bucket"`
	DLQStream *string `yaml:"dlq_stream"`
}

type yamlMongoConfig struct {
	URI           *string `yaml:"uri"`
	DB            *string `yaml:"db"`
	EventsTTLDays *int    `yaml:"events_ttl_days"`
}

type yamlS3Config struct {
	Endpoint        *string `yaml:"endpoint"`
	Region          *string `yaml:"region"`
	Bucket          *string `yaml:"bucket"`
	AccessKeyID     *string `yaml:"access_key_id"`
	SecretAccessKey *string `yaml:"secret_access_key"`
	UsePathStyle    *bool   `yaml:"use_path_style"`
}

type yamlRunnerConfig struct {
	WorkDir     *string `yaml:"workdir"`
	Concurrency *int    `yaml:"concurrency"`
	Heartbeat   *string `yaml:"heartbeat"`
	LockTTL     *string `yaml:"lock_ttl"`
}

type yamlServerConfig struct {
	HealthzPort *int `yaml:"healthz_port"`
}

type yamlMetricsConfig struct {
	Port *int `yaml:"port"`
}

type yamlLogConfig struct {
	Format *string `yaml:"format"`
	Level  *string `yaml:"level"`
}

func (y *yamlConfig) applyTo(cfg *Config) error {
	if y.Mode != nil {
		cfg.Mode = Mode(*y.Mode)
	}
	if y.NATS != nil {
		applyString(y.NATS.URL, &cfg.NATS.URL)
		applyString(y.NATS.Stream, &cfg.NATS.Stream)
		applyString(y.NATS.KVBucket, &cfg.NATS.KVBucket)
		applyString(y.NATS.DLQStream, &cfg.NATS.DLQStream)
	}
	if y.Mongo != nil {
		applyString(y.Mongo.URI, &cfg.Mongo.URI)
		applyString(y.Mongo.DB, &cfg.Mongo.DB)
		applyInt(y.Mongo.EventsTTLDays, &cfg.Mongo.EventsTTLDays)
	}
	if y.S3 != nil {
		applyString(y.S3.Endpoint, &cfg.S3.Endpoint)
		applyString(y.S3.Region, &cfg.S3.Region)
		applyString(y.S3.Bucket, &cfg.S3.Bucket)
		applyString(y.S3.AccessKeyID, &cfg.S3.AccessKeyID)
		applyString(y.S3.SecretAccessKey, &cfg.S3.SecretAccessKey)
		if y.S3.UsePathStyle != nil {
			cfg.S3.UsePathStyle = *y.S3.UsePathStyle
		}
	}
	if y.Runner != nil {
		applyString(y.Runner.WorkDir, &cfg.Runner.WorkDir)
		applyInt(y.Runner.Concurrency, &cfg.Runner.Concurrency)
		if y.Runner.Heartbeat != nil {
			d, err := time.ParseDuration(*y.Runner.Heartbeat)
			if err != nil {
				return fmt.Errorf("runner.heartbeat: %w", err)
			}
			cfg.Runner.Heartbeat = d
		}
		if y.Runner.LockTTL != nil {
			d, err := time.ParseDuration(*y.Runner.LockTTL)
			if err != nil {
				return fmt.Errorf("runner.lock_ttl: %w", err)
			}
			cfg.Runner.LockTTL = d
		}
	}
	if y.Server != nil {
		applyInt(y.Server.HealthzPort, &cfg.Server.HealthzPort)
	}
	if y.Metrics != nil {
		applyInt(y.Metrics.Port, &cfg.Metrics.Port)
	}
	if y.Log != nil {
		if y.Log.Format != nil {
			cfg.Log.Format = LogFormat(*y.Log.Format)
		}
		applyString(y.Log.Level, &cfg.Log.Level)
	}
	return nil
}

func applyString(src *string, dst *string) {
	if src != nil {
		*dst = *src
	}
}

func applyInt(src *int, dst *int) {
	if src != nil {
		*dst = *src
	}
}
