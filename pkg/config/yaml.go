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
	Sandbox *yamlSandboxConfig `yaml:"sandbox"`
	NATS    *yamlNATSConfig    `yaml:"nats"`
	Mongo   *yamlMongoConfig   `yaml:"mongo"`
	S3      *yamlS3Config      `yaml:"s3"`
	Runner  *yamlRunnerConfig  `yaml:"runner"`
	Server  *yamlServerConfig  `yaml:"server"`
	Metrics *yamlMetricsConfig `yaml:"metrics"`
	Log     *yamlLogConfig     `yaml:"log"`
	Auth    *yamlAuthConfig    `yaml:"auth"`
	Alerts  *yamlAlertsConfig  `yaml:"alerts"`
}

type yamlAlertsConfig struct {
	Webhook      *yamlWebhookAlertConfig `yaml:"webhook"`
	Desktop      *yamlDesktopAlertConfig `yaml:"desktop"`
	StallTimeout *string                 `yaml:"stall_timeout"`
}

type yamlWebhookAlertConfig struct {
	URL *string `yaml:"url"`
}

type yamlDesktopAlertConfig struct {
	Enabled *bool `yaml:"enabled"`
}

type yamlSandboxConfig struct {
	Default   *string `yaml:"default"`
	HostState *string `yaml:"host_state"`
}

type yamlAuthConfig struct {
	JWTSecret           *string                 `yaml:"jwt_secret"`
	SecretsKey          *string                 `yaml:"secrets_key"`
	AccessTTL           *string                 `yaml:"access_ttl"`
	RefreshTTL          *string                 `yaml:"refresh_ttl"`
	BootstrapAdminEmail *string                 `yaml:"bootstrap_admin_email"`
	SignupMode          *string                 `yaml:"signup_mode"`
	PublicURL           *string                 `yaml:"public_url"`
	CookieDomain        *string                 `yaml:"cookie_domain"`
	CookieSecure        *bool                   `yaml:"cookie_secure"`
	OIDC                *yamlOIDCConfig         `yaml:"oidc"`
	OAuthForfait        *yamlOAuthForfaitConfig `yaml:"oauth_forfait"`
}

type yamlOIDCConfig struct {
	Google                *yamlOIDCProviderConfig `yaml:"google"`
	GitHub                *yamlOIDCProviderConfig `yaml:"github"`
	Generic               *yamlOIDCProviderConfig `yaml:"generic"`
	GitHubUngrantedPolicy *string                 `yaml:"github_ungranted_policy"`
}

type yamlOIDCProviderConfig struct {
	Enabled      *bool     `yaml:"enabled"`
	IssuerURL    *string   `yaml:"issuer_url"`
	ClientID     *string   `yaml:"client_id"`
	ClientSecret *string   `yaml:"client_secret"`
	Scopes       *[]string `yaml:"scopes"`
	DisplayName  *string   `yaml:"display_name"`
}

type yamlOAuthForfaitConfig struct {
	AnthropicClientID *string `yaml:"anthropic_client_id"`
	CodexClientID     *string `yaml:"codex_client_id"`
}

type yamlNATSConfig struct {
	URL       *string `yaml:"url"`
	Stream    *string `yaml:"stream"`
	KVBucket  *string `yaml:"kv_bucket"`
	DLQStream *string `yaml:"dlq_stream"`

	MaxAckPending *int    `yaml:"max_ack_pending"`
	AckWait       *string `yaml:"ack_wait"`
	MaxDeliver    *int    `yaml:"max_deliver"`
	MaxAge        *string `yaml:"max_age"`
	DLQMaxAge     *string `yaml:"dlq_max_age"`
	MaxPayload    *int    `yaml:"max_payload"`
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
	WorkDir             *string `yaml:"workdir"`
	Concurrency         *int    `yaml:"concurrency"`
	Heartbeat           *string `yaml:"heartbeat"`
	LockTTL             *string `yaml:"lock_ttl"`
	DrainMode           *string `yaml:"drain_mode"`
	DrainTimeout        *string `yaml:"drain_timeout"`
	SchemaMismatchDelay *string `yaml:"schema_mismatch_delay"`
}

type yamlServerConfig struct {
	ShutdownDelay    *string `yaml:"shutdown_delay"`
	ShutdownTeardown *string `yaml:"shutdown_teardown"`
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
		applyInt(y.NATS.MaxAckPending, &cfg.NATS.MaxAckPending)
		applyInt(y.NATS.MaxDeliver, &cfg.NATS.MaxDeliver)
		applyInt(y.NATS.MaxPayload, &cfg.NATS.MaxPayload)
		if y.NATS.AckWait != nil {
			d, err := time.ParseDuration(*y.NATS.AckWait)
			if err != nil {
				return fmt.Errorf("nats.ack_wait: %w", err)
			}
			cfg.NATS.AckWait = d
		}
		if y.NATS.MaxAge != nil {
			d, err := time.ParseDuration(*y.NATS.MaxAge)
			if err != nil {
				return fmt.Errorf("nats.max_age: %w", err)
			}
			cfg.NATS.MaxAge = d
		}
		if y.NATS.DLQMaxAge != nil {
			d, err := time.ParseDuration(*y.NATS.DLQMaxAge)
			if err != nil {
				return fmt.Errorf("nats.dlq_max_age: %w", err)
			}
			cfg.NATS.DLQMaxAge = d
		}
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
		applyString(y.Runner.DrainMode, &cfg.Runner.DrainMode)
		if y.Runner.DrainTimeout != nil {
			d, err := time.ParseDuration(*y.Runner.DrainTimeout)
			if err != nil {
				return fmt.Errorf("runner.drain_timeout: %w", err)
			}
			cfg.Runner.DrainTimeout = d
		}
		if y.Runner.SchemaMismatchDelay != nil {
			d, err := time.ParseDuration(*y.Runner.SchemaMismatchDelay)
			if err != nil {
				return fmt.Errorf("runner.schema_mismatch_delay: %w", err)
			}
			cfg.Runner.SchemaMismatchDelay = d
		}
	}
	if y.Server != nil {
		if y.Server.ShutdownDelay != nil {
			d, err := time.ParseDuration(*y.Server.ShutdownDelay)
			if err != nil {
				return fmt.Errorf("server.shutdown_delay: %w", err)
			}
			cfg.Server.ShutdownDelay = d
		}
		if y.Server.ShutdownTeardown != nil {
			d, err := time.ParseDuration(*y.Server.ShutdownTeardown)
			if err != nil {
				return fmt.Errorf("server.shutdown_teardown: %w", err)
			}
			cfg.Server.ShutdownTeardown = d
		}
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
	if y.Auth != nil {
		applyString(y.Auth.JWTSecret, &cfg.Auth.JWTSecret)
		applyString(y.Auth.SecretsKey, &cfg.Auth.SecretsKey)
		if y.Auth.AccessTTL != nil {
			d, err := time.ParseDuration(*y.Auth.AccessTTL)
			if err != nil {
				return fmt.Errorf("auth.access_ttl: %w", err)
			}
			cfg.Auth.AccessTTL = d
		}
		if y.Auth.RefreshTTL != nil {
			d, err := time.ParseDuration(*y.Auth.RefreshTTL)
			if err != nil {
				return fmt.Errorf("auth.refresh_ttl: %w", err)
			}
			cfg.Auth.RefreshTTL = d
		}
		applyString(y.Auth.BootstrapAdminEmail, &cfg.Auth.BootstrapAdminEmail)
		applyString(y.Auth.SignupMode, &cfg.Auth.SignupMode)
		applyString(y.Auth.PublicURL, &cfg.Auth.PublicURL)
		applyString(y.Auth.CookieDomain, &cfg.Auth.CookieDomain)
		if y.Auth.CookieSecure != nil {
			cfg.Auth.CookieSecure = *y.Auth.CookieSecure
		}
		if y.Auth.OIDC != nil {
			applyOIDCProvider(y.Auth.OIDC.Google, &cfg.Auth.OIDC.Google)
			applyOIDCProvider(y.Auth.OIDC.GitHub, &cfg.Auth.OIDC.GitHub)
			applyOIDCProvider(y.Auth.OIDC.Generic, &cfg.Auth.OIDC.Generic)
			applyString(y.Auth.OIDC.GitHubUngrantedPolicy, &cfg.Auth.OIDC.GitHubUngrantedPolicy)
		}
		if y.Auth.OAuthForfait != nil {
			applyString(y.Auth.OAuthForfait.AnthropicClientID, &cfg.Auth.OAuthForfait.AnthropicClientID)
			applyString(y.Auth.OAuthForfait.CodexClientID, &cfg.Auth.OAuthForfait.CodexClientID)
		}
	}
	if y.Sandbox != nil {
		applyString(y.Sandbox.Default, &cfg.Sandbox.Default)
		applyString(y.Sandbox.HostState, &cfg.Sandbox.HostState)
	}
	if y.Alerts != nil {
		if y.Alerts.Webhook != nil {
			applyString(y.Alerts.Webhook.URL, &cfg.Alerts.Webhook.URL)
		}
		if y.Alerts.Desktop != nil && y.Alerts.Desktop.Enabled != nil {
			cfg.Alerts.Desktop.Enabled = *y.Alerts.Desktop.Enabled
		}
		if y.Alerts.StallTimeout != nil {
			d, err := time.ParseDuration(*y.Alerts.StallTimeout)
			if err != nil {
				return fmt.Errorf("alerts.stall_timeout: %w", err)
			}
			cfg.Alerts.StallTimeout = d
		}
	}
	return nil
}

func applyOIDCProvider(src *yamlOIDCProviderConfig, dst *OIDCProviderConfig) {
	if src == nil {
		return
	}
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	applyString(src.IssuerURL, &dst.IssuerURL)
	applyString(src.ClientID, &dst.ClientID)
	applyString(src.ClientSecret, &dst.ClientSecret)
	if src.Scopes != nil {
		dst.Scopes = *src.Scopes
	}
	applyString(src.DisplayName, &dst.DisplayName)
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
