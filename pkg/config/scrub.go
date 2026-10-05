package config

import (
	"fmt"
	"os"
)

// PlatformSecretEnv are the process-environment names that carry a cloud
// deployment's own credentials, connection strings and operator identity. The
// server and the runner read each once, while booting (Load, the sealer, the
// forge App, the mailer, the run service, errtrack.Init, tracing.Init — whose
// OTLP client reads its headers at construction, not per export);
// ScrubPlatformSecrets then removes them, so nothing that runs later in the
// process — workflow text, a tool or agent shell, a schedule guard, an MCP
// server, any child that inherits os.Environ() — can read them. Provider keys
// are not among them: a run spends those.
var PlatformSecretEnv = []string{
	"ITERION_SECRETS_KEY",
	"ITERION_SECRETS_KEYS",
	"ITERION_SECRETS_KEY_ID",
	"ITERION_JWT_SECRET",
	"ITERION_MONGO_URI",
	"ITERION_NATS_URL",
	"ITERION_REDIS_URL",
	"ITERION_REDIS_PASSWORD",
	"ITERION_REDIS_SENTINEL_PASSWORD",
	"ITERION_S3_ACCESS_KEY_ID",
	"ITERION_S3_SECRET_ACCESS_KEY",
	"ITERION_SMTP_PASSWORD",
	"ITERION_FORGE_GITHUB_APP_PRIVATE_KEY",
	"ITERION_FORGE_GITHUB_APP_CLIENT_SECRET",
	"ITERION_OIDC_GENERIC_CLIENT_SECRET",
	"ITERION_OIDC_GITHUB_CLIENT_SECRET",
	"ITERION_OIDC_GOOGLE_CLIENT_SECRET",
	"ITERION_WEBPUSH_VAPID_PRIVATE_KEY",
	"ITERION_BOOTSTRAP_ADMIN_PASSWORD",
	"ITERION_COMPLETION_WEBHOOK_SECRET",
	"ITERION_ALERTS_WEBHOOK_URL",
	"ITERION_SMTP_USERNAME",
	"ITERION_BOOTSTRAP_ADMIN_EMAIL",
	// Not ITERION_-prefixed, and platform-owned all the same: the DSN is a
	// write credential for the deployment's error tracker, and an OTLP header
	// block is where a collector's auth goes.
	"SENTRY_DSN",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
}

// ScrubPlatformSecrets removes PlatformSecretEnv from the process environment
// and marks the process non-dumpable: unsetting rewrites the process's copy
// of its environment, not the boot block /proc/<pid>/environ serves, which a
// child running as the same user could otherwise still read. Called by a
// cloud server or runner once its last boot reader is done.
func ScrubPlatformSecrets() error {
	for _, name := range PlatformSecretEnv {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("config: unset %s: %w", name, err)
		}
	}
	return setNonDumpable()
}
