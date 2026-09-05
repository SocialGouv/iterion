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
//
// An empty resolved value is an ERROR, not an accepted secret: the server's
// rotate/create path treats an empty `secret` as "leave unchanged", so a
// `rotate` fed an empty source (an unset-then-empty env, a truncated file,
// a bare newline on stdin) would return 200 OK with the OLD key still live
// — the operator believes they rotated a compromised credential when they
// did not. Rejecting here closes every input path and every caller at once.
func ReadSecretValue(fromEnv, fromFile string, stdinOK bool) (string, error) {
	var v string
	switch {
	case fromEnv != "":
		val, ok := os.LookupEnv(fromEnv)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", fromEnv)
		}
		v = val
	case fromFile != "":
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return "", err
		}
		// CRLF too: a file written on Windows leaves a trailing \r, which
		// the server's shape gate refuses as a control character with a
		// message about a terminal transcript — right refusal, misleading
		// cause, for a file that is perfectly fine.
		v = strings.TrimRight(string(b), "\r\n")
	case stdinOK:
		r := bufio.NewReader(os.Stdin)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		v = strings.TrimRight(line, "\r\n")
	default:
		return "", fmt.Errorf("provide the value via --from-env <VAR>, --from-file <path>, or pipe it on stdin")
	}
	if v == "" {
		return "", fmt.Errorf("secret value is empty — a create/rotate with an empty secret is a silent no-op; check the source")
	}
	return v, nil
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
