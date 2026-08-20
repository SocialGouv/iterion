package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// ReadSecretValue resolves a secret's value from --from-env, @file, or
// stdin — never from a positional argument (process lists leak argv).
func ReadSecretValue(fromEnv, fromFile string, stdinOK bool) (string, error) {
	switch {
	case fromEnv != "":
		v, ok := os.LookupEnv(fromEnv)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", fromEnv)
		}
		return v, nil
	case fromFile != "":
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	case stdinOK:
		r := bufio.NewReader(os.Stdin)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		return strings.TrimRight(line, "\n"), nil
	default:
		return "", fmt.Errorf("provide the value via --from-env <VAR>, --from-file <path>, or pipe it on stdin")
	}
}

// remoteSecretsBase returns the REST prefix for a scope: the active
// team's store, the personal (/api/me) store, or the super-admin
// platform store (/api/admin/llm — api-keys only; the deployment's own
// fallback credentials).
func remoteSecretsBase(ctx context.Context, c *RemoteClient, scope, teamFlag, resource string) (string, error) {
	switch scope {
	case "", "team":
		team, err := c.ResolveTeam(ctx, teamFlag)
		if err != nil {
			return "", err
		}
		return "/api/teams/" + team + "/" + resource, nil
	case "me":
		return "/api/me/" + resource, nil
	case "platform":
		return "/api/admin/llm/" + resource, nil
	default:
		return "", fmt.Errorf("invalid --scope %q (want team|me|platform)", scope)
	}
}

func RemoteSecretsSet(ctx context.Context, c *RemoteClient, p *Printer, scope, teamFlag, name, value string) error {
	base, err := remoteSecretsBase(ctx, c, scope, teamFlag, "secrets")
	if err != nil {
		return err
	}
	raw, err := c.Call(ctx, "POST", base, map[string]string{"name": name, "secret": value}, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

func RemoteSecretsRotate(ctx context.Context, c *RemoteClient, p *Printer, scope, teamFlag, secretID, value string) error {
	base, err := remoteSecretsBase(ctx, c, scope, teamFlag, "secrets")
	if err != nil {
		return err
	}
	raw, err := c.Call(ctx, "PATCH", base+"/"+secretID, map[string]string{"secret": value}, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

func RemoteAPIKeysCreate(ctx context.Context, c *RemoteClient, p *Printer, scope, teamFlag, provider, name, value string, isDefault bool) error {
	base, err := remoteSecretsBase(ctx, c, scope, teamFlag, "api-keys")
	if err != nil {
		return err
	}
	req := map[string]any{"provider": provider, "name": name, "secret": value}
	if isDefault {
		req["is_default"] = true
	}
	raw, err := c.Call(ctx, "POST", base, req, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteAPIKeysRotate replaces a key's sealed value in place — for the
// platform scope this is the one-call rotation that used to be a k8s
// secret edit + redeploy. New launches and resumes pick the fresh value
// immediately; in-flight runs keep their sealed snapshot until they
// finish or fail-resume.
func RemoteAPIKeysRotate(ctx context.Context, c *RemoteClient, p *Printer, scope, teamFlag, keyID, value string) error {
	base, err := remoteSecretsBase(ctx, c, scope, teamFlag, "api-keys")
	if err != nil {
		return err
	}
	raw, err := c.Call(ctx, "PATCH", base+"/"+keyID, map[string]string{"secret": value}, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}
