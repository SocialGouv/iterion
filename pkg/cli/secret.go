package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SecretOptions carries the shared inputs for the `iterion secret` subcommands.
// StoreDir is the per-project store dir (the `.iterion` the run store uses);
// empty resolves it from the working directory. Project selects the per-project
// layer over the machine-global one for set/remove.
type SecretOptions struct {
	Name     string
	Kind     string
	FromEnv  string
	Project  bool
	Hosts    []string
	HostsSet bool // whether --hosts was explicitly passed (distinguish "clear" from "leave")
	StoreDir string
}

// buildLocalSecrets resolves the layered store + sealer for the CLI secret
// commands, mirroring the run path (global + optional project override, sealed
// with a keychain/keyfile master key).
func buildLocalSecrets(opts SecretOptions) (*secrets.LayeredGenericSecretStore, secrets.Sealer, error) {
	projectDir := store.ResolveStoreDir(cwd(), opts.StoreDir)
	st, err := LocalSecretStores(projectDir)
	if err != nil {
		return nil, nil, err
	}
	warnLog := iterlog.New(iterlog.LevelWarn, os.Stderr)
	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), warnLog.Warn)
	if err != nil {
		return nil, nil, err
	}
	return st, sealer, nil
}

func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

func (o SecretOptions) scope() string {
	if o.Project {
		return "project"
	}
	return "global"
}

// RunSecretSet creates or rotates (upsert-by-name) a local secret. The value is
// read — never from argv — via --from-env, a stdin pipe, or a masked TTY
// prompt. The value is AES-GCM sealed before it touches disk and is never
// echoed back.
func RunSecretSet(p *Printer, opts SecretOptions) error {
	name := strings.TrimSpace(opts.Name)
	if !secrets.ValidGenericSecretName(name) {
		return fmt.Errorf("invalid secret name %q (use [A-Za-z_][A-Za-z0-9_]*, ≤128 chars)", opts.Name)
	}
	value, err := readSecretValue(opts.FromEnv, name)
	if err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("empty secret value")
	}
	if err := validateSecretShape(opts.Kind, name, value); err != nil {
		return err
	}

	st, sealer, err := buildLocalSecrets(opts)
	if err != nil {
		return err
	}
	// Effective scope: "project" degrades to "global" when no project layer is
	// active, so the reported scope matches where the secret actually lands.
	scope := opts.scope()
	if scope == "project" {
		if _, active := st.Project(); !active {
			scope = "global"
		}
	}
	// Atomic upsert-by-name under the store's cross-process lock. On rotate,
	// --hosts overwrites the egress lock only when explicitly passed
	// (opts.HostsSet); otherwise it is preserved (never silently broadened).
	rec, created, err := st.ForScope(scope).UpsertByName(sealer, name, value, opts.Hosts, opts.HostsSet)
	if err != nil {
		return err
	}
	verb := "Rotated"
	if created {
		verb = "Stored"
	}
	p.Line("%s secret %q (%s scope, last4 %s)", verb, name, scope, rec.Last4)
	return nil
}

// validateSecretShape is the CLI's half of the ingestion gate the API
// paths run (secrets.ValidateAPIKeyShape / ValidateTokenShape): a value
// that could not possibly authenticate is refused at the paste, not
// discovered as a provider 401 in the middle of a run hours later.
//
// The local store is name-keyed and carries no kind of its own, so the
// shape is what the operator names with --kind, or what the value itself
// says (a PEM header, a JSON opener, else a bare token). `--kind raw` is
// the explicit opt-out for a value that is none of the three — the
// refusal names it, so the remedy travels with the message.
func validateSecretShape(kind, name, value string) error {
	shape := secrets.InferSecretShapeKind(value)
	if strings.TrimSpace(kind) != "" {
		parsed, err := secrets.ParseSecretShapeKind(kind)
		if err != nil {
			return err
		}
		shape = parsed
	}
	if err := secrets.ValidateSecretShape(shape, name, value); err != nil {
		var se *secrets.ShapeError
		if errors.As(err, &se) {
			return fmt.Errorf("%s (read as --kind %s; pass --kind raw to store it unchecked)", se.Error(), shape)
		}
		return err
	}
	return nil
}

// RunSecretList prints every local secret (both layers) with its scope and
// last4 — never the value.
func RunSecretList(p *Printer, opts SecretOptions) error {
	st, _, err := buildLocalSecrets(opts)
	if err != nil {
		return err
	}
	scoped, err := st.ListScoped(context.Background(), secrets.LocalScopeTeam, "")
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		type view struct {
			Name         string   `json:"name"`
			Scope        string   `json:"scope"`
			Last4        string   `json:"last4"`
			AllowedHosts []string `json:"allowed_hosts,omitempty"`
		}
		out := make([]view, 0, len(scoped))
		for _, sc := range scoped {
			out = append(out, view{Name: sc.Secret.Name, Scope: sc.Scope, Last4: sc.Secret.Last4, AllowedHosts: sc.Secret.AllowedHosts})
		}
		p.JSON(out)
		return nil
	}
	if len(scoped) == 0 {
		p.Line("No local secrets. Add one with: iterion secret set <NAME>")
		return nil
	}
	rows := make([][]string, 0, len(scoped))
	for _, sc := range scoped {
		rows = append(rows, []string{sc.Secret.Name, sc.Scope, sc.Secret.Last4, strings.Join(sc.Secret.AllowedHosts, ",")})
	}
	p.Table([]string{"NAME", "SCOPE", "LAST4", "HOSTS"}, rows)
	return nil
}

// RunSecretRemove deletes a local secret by name (searching the project layer
// first, then global).
func RunSecretRemove(p *Printer, opts SecretOptions) error {
	name := strings.TrimSpace(opts.Name)
	st, _, err := buildLocalSecrets(opts)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if proj, active := st.Project(); active {
		if rec, ok := proj.GetByName(name); ok {
			if err := proj.Delete(ctx, rec.ID); err != nil {
				return err
			}
			p.Line("Removed secret %q (project scope)", name)
			return nil
		}
	}
	if rec, ok := st.Global().GetByName(name); ok {
		if err := st.Global().Delete(ctx, rec.ID); err != nil {
			return err
		}
		p.Line("Removed secret %q (global scope)", name)
		return nil
	}
	return fmt.Errorf("no local secret named %q", name)
}

// readSecretValue obtains the plaintext without ever placing it on the command
// line: from --from-env, else a stdin pipe, else a masked TTY prompt.
func readSecretValue(fromEnv, name string) (string, error) {
	if fromEnv != "" {
		v, ok := os.LookupEnv(fromEnv)
		if !ok {
			return "", fmt.Errorf("--from-env %s: environment variable is not set", fromEnv)
		}
		return v, nil
	}
	if !IsTTY() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		// Strip a SINGLE trailing newline (the shell/heredoc/echo artifact),
		// not every trailing newline — a value that legitimately ends in "\n"
		// (e.g. a PEM/SSH private key) must survive intact.
		return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), nil
	}
	fmt.Fprintf(os.Stderr, "Value for %s (input hidden): ", name)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
