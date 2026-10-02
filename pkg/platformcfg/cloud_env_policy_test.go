package platformcfg

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/config"
)

// On a cloud process, workflow text reads no credential-shaped name from the
// process environment; the operator's knobs, the engine-supplied names and
// the deployment's escape hatch still resolve.
func TestCloudProcessEnvPolicy(t *testing.T) {
	t.Setenv(EnvCloudEnvPassthrough, " GITHUB_TOKEN ,")
	allowed := CloudProcessEnvPolicy()
	for _, name := range []string{
		"ITERION_SECRETS_KEY", "ITERION_JWT_SECRET", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "ITERION_ALERTS_WEBHOOK_URL", "DB_PASSWORD",
		"ITERION_FORGE_GITHUB_APP_PRIVATE_KEY", "api_key", "SENTRY_DSN",
		// A header block carries whatever authenticates the call it rides on;
		// a proxy URL carries its own userinfo.
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"ANTHROPIC_CUSTOM_HEADERS", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy",
		// Shape says nothing; the scrub list knows them.
		"ITERION_MONGO_URI", "ITERION_NATS_URL", "ITERION_REDIS_URL",
		"ITERION_BOOTSTRAP_ADMIN_EMAIL", "ITERION_SMTP_USERNAME",
	} {
		if allowed(name) {
			t.Errorf("workflow text may read %s from a cloud process", name)
		}
	}
	for _, name := range []string{
		"ITERION_VIBE_EFFORT_CLAUDE", "ITERION_SEC_PATCH_EFFORT_AUTHOR", "RESCUE_PROVIDER",
		"PROJECT_DIR", "HOME", "ITERION_DEFAULT_BACKEND", "GITHUB_TOKEN",
		// Neighbours of the new segments that name no credential.
		"OTEL_EXPORTER_OTLP_ENDPOINT", "ITERION_S3_ENDPOINT", "ITERION_MONGO_DB", "NO_PROXYING",
	} {
		if !allowed(name) {
			t.Errorf("workflow text may no longer read %s on a cloud process", name)
		}
	}
}

// Every name the scrub removes is also one workflow text may not read: the
// two halves answer for the same set, so a process that installs the policy
// and forgets the scrub still refuses them.
func TestCloudProcessEnvPolicyRefusesEveryScrubbedName(t *testing.T) {
	allowed := CloudProcessEnvPolicy()
	for _, name := range config.PlatformSecretEnv {
		if allowed(name) {
			t.Errorf("the policy lets workflow text read %s, which the scrub removes", name)
		}
	}
}
