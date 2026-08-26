package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// SecurityReadSecretName is the well-known team generic secret holding the
// org-wide Dependabot-alerts read tokens: a JSON map {org_login: token},
// merged across every github_app connection of the tenant that opted into
// SecurityReadEnabled. Bots (vuln-watch) declare a secret with this name and
// read the map from its file mount; resolution rides the ordinary
// teamByName tier, so a deployment WITHOUT the GitHub App flow can fill the
// same secret by hand with fine-grained PATs — the shape is the contract,
// not the mint.
const SecurityReadSecretName = "dependabot_tokens"

// securityReadCreatedBy marks a dependabot_tokens secret CREATED by iterion's
// security-read flow (vs hand-set by an operator). Deleting an emptied map is
// only allowed on iterion's own secret — destroying an operator's hand-set
// secret on disable would silently replace their explicit choice.
const securityReadCreatedBy = "forge-security-read"

// ErrSecurityReadMalformed marks a dependabot_tokens secret whose plaintext
// is not the JSON map the contract requires — an operator's hand-set value,
// typically a bare PAT. Callers that can act on it surface it; the ones for
// which it is incidental (disconnecting) may skip past it. Distinguishing it
// from a store/seal failure matters: those leave a LIVE token behind.
var ErrSecurityReadMalformed = errors.New("forge: malformed security-read secret")

// ErrSecurityReadNoOrgKey marks a connection that cannot be filed in the map
// because nothing names the org it operates on. Like a missing grant this is
// permanent — retrying re-derives the same nothing — so the refresh worker
// records it once instead of re-minting against the forge on every tick.
var ErrSecurityReadNoOrgKey = errors.New("forge: connection has no org to key its security-read token by")

// fingerprintGuardedStore is the optional CAS capability a GenericSecretStore
// may implement. The dependabot_tokens map is the first generic secret SHARED
// across connections and rewritten by every replica's refresh worker;
// last-writer-wins on it can resurrect a just-revoked org token. Stores
// without the capability (single-process local file store) fall back to a
// plain Update.
type fingerprintGuardedStore interface {
	UpdateIfFingerprint(ctx context.Context, rec secrets.GenericSecret, expected string) error
}

// guardedUpdate applies rec through the store's CAS capability when it has
// one, retrying once on a concurrent-write conflict via reread (the caller's
// mutate func recomputes the record from the fresh read).
func guardedUpdate(ctx context.Context, st secrets.GenericSecretStore, rec secrets.GenericSecret, expected string) error {
	if g, ok := st.(fingerprintGuardedStore); ok {
		return g.UpdateIfFingerprint(ctx, rec, expected)
	}
	return st.Update(ctx, rec)
}

// securityReadTokens decodes the secret plaintext. A missing/empty secret
// decodes to an empty map; anything non-JSON is an explicit error (a
// hand-set secret with a syntax error must surface, not be silently
// replaced).
func securityReadTokens(plaintext []byte) (map[string]string, error) {
	m := map[string]string{}
	if len(plaintext) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(plaintext, &m); err != nil {
		return nil, fmt.Errorf("%w: %s secret is not a JSON map of org→token: %w",
			ErrSecurityReadMalformed, SecurityReadSecretName, err)
	}
	return m, nil
}

func encodeSecurityReadTokens(m map[string]string) ([]byte, error) {
	// Stable key order so re-seals of identical content are comparable.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(m))
	for _, k := range keys {
		ordered[k] = m[k]
	}
	return json.Marshal(ordered)
}

// findSecurityReadSecret returns the tenant's team-scoped secret named
// SecurityReadSecretName, or ok=false when none exists.
func findSecurityReadSecret(ctx context.Context, st secrets.GenericSecretStore, tenantID string) (secrets.GenericSecret, bool, error) {
	list, err := st.ListByTeam(ctx, tenantID, "")
	if err != nil {
		return secrets.GenericSecret{}, false, fmt.Errorf("forge: list team secrets: %w", err)
	}
	for _, s := range list {
		if s.Name == SecurityReadSecretName && s.ScopeUserID == "" {
			return s, true, nil
		}
	}
	return secrets.GenericSecret{}, false, nil
}

// securityReadOrgKey is the map key a connection's token is filed under: the
// installation's account login (the GitHub org), lowercased so the bot's
// config match is case-insensitive.
func securityReadOrgKey(conn *Connection) (string, error) {
	// A github_app connection authenticates AS the App (AccountLogin is the
	// bot handle) but operates ON the org — and the bot's config names orgs.
	// Filing the token under the bot handle produces a map no run can look
	// up, and the failure only shows as "no Dependabot token for org X" an
	// hour later, so refuse instead.
	if conn.Kind == KindGitHubApp {
		org := strings.ToLower(strings.TrimSpace(conn.InstallationAccount))
		if org == "" {
			return "", fmt.Errorf("%w: connection %s has no installation account recorded (reconnect, or open the connection's health view to refresh it)", ErrSecurityReadNoOrgKey, conn.ID)
		}
		return org, nil
	}
	org := strings.ToLower(strings.TrimSpace(conn.AccountLogin))
	if org == "" {
		return "", fmt.Errorf("%w: connection %s has no account login", ErrSecurityReadNoOrgKey, conn.ID)
	}
	return org, nil
}

// The map is keyed by org alone (the bot's config names orgs, not hosts), so
// two connections claiming the same org would overwrite each other every
// cycle. That is guarded at the ENABLE endpoint (securityReadOrgCollision),
// which is a read-then-write and therefore best-effort: two admins enabling
// concurrently on two replicas can both pass it. The failure it leaves is
// flapping tokens for one org, not a leak across tenants — an atomic guard
// would need the write path to see the connection store, which it
// deliberately does not.

// UpsertSecurityReadToken merges {org_login: token} into the tenant's
// dependabot_tokens secret, creating it (team-scoped, egress-pinned to the
// forge host, marked as iterion-created) on first use. Shared by the refresh
// worker's hourly re-mint and the enable endpoint's immediate mint, so both
// write the same shape. Writes go through the store's fingerprint CAS when
// available (one reread-retry): every replica runs a refresh worker, and a
// lost update here can resurrect a just-revoked org token.
func UpsertSecurityReadToken(ctx context.Context, st secrets.GenericSecretStore, sealer secrets.Sealer, conn *Connection, token string, now time.Time) error {
	org, err := securityReadOrgKey(conn)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		gs, ok, err := findSecurityReadSecret(ctx, st, conn.TenantID)
		if err != nil {
			return err
		}
		tokens := map[string]string{}
		if ok {
			plain, err := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
			if err != nil {
				return fmt.Errorf("forge: open %s secret: %w", SecurityReadSecretName, err)
			}
			if tokens, err = securityReadTokens(plain); err != nil {
				return err
			}
		}
		tokens[org] = token
		// Remember where it landed, so withdrawal never has to guess (see
		// Connection.SecurityReadOrgKey).
		conn.SecurityReadOrgKey = org
		encoded, err := encodeSecurityReadTokens(tokens)
		if err != nil {
			return err
		}
		if !ok {
			gs = secrets.GenericSecret{
				ID:          secrets.NewGenericSecretID(),
				TenantID:    conn.TenantID,
				ScopeTeamID: conn.TenantID,
				Name:        SecurityReadSecretName,
				// Egress lock: the map only ever needs to reach the forge API.
				AllowedHosts: forgeTokenEgressHosts(conn),
				CreatedBy:    securityReadCreatedBy,
				CreatedAt:    now,
			}
			sealed, err := secrets.SealGenericSecret(sealer, gs.ID, encoded)
			if err != nil {
				return err
			}
			gs.SealedSecret = sealed
			gs.Fingerprint = secrets.FingerprintSHA256(string(encoded))
			if err := st.Create(ctx, gs); err != nil {
				return fmt.Errorf("forge: create %s secret: %w", SecurityReadSecretName, err)
			}
			// The create path cannot CAS (there is nothing to compare
			// against) and the store has no unique (team, name) index, so two
			// replicas first-minting concurrently would leave TWO secrets of
			// this name — one shadowing the other, its org's token filed
			// where nothing reads it, and the bot hard-failing on a missing
			// org with no server-side signal. Re-read: if another writer's
			// record is the one that resolves, fold our entry into it and
			// drop ours.
			winner, found, ferr := findSecurityReadSecret(ctx, st, conn.TenantID)
			if ferr != nil || !found || winner.ID == gs.ID {
				return nil
			}
			if err := st.Delete(ctx, gs.ID); err != nil {
				return fmt.Errorf("forge: drop duplicate %s secret: %w", SecurityReadSecretName, err)
			}
			if attempt == 0 {
				continue // merge into the winner through the ordinary CAS path
			}
			return nil
		}
		expected := gs.Fingerprint
		// The egress lock must hold on EVERY write, not only at creation: a
		// hand-created secret may carry no pin at all, and a second
		// connection on another forge host must add its own host. Union —
		// an operator's own pins are kept, never removed.
		gs.AllowedHosts = unionHosts(gs.AllowedHosts, forgeTokenEgressHosts(conn))
		sealed, err := secrets.SealGenericSecret(sealer, gs.ID, encoded)
		if err != nil {
			return err
		}
		gs.SealedSecret = sealed
		gs.Fingerprint = secrets.FingerprintSHA256(string(encoded))
		err = guardedUpdate(ctx, st, gs, expected)
		if errors.Is(err, secrets.ErrGenericSecretConflict) && attempt == 0 {
			continue // a concurrent writer landed first — reread and remerge
		}
		if err != nil {
			return fmt.Errorf("forge: update %s secret: %w", SecurityReadSecretName, err)
		}
		return nil
	}
}

func unionHosts(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, h := range append(append([]string{}, a...), b...) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// RemoveSecurityReadToken drops the connection's org entry from the
// dependabot_tokens secret. When the map empties, the secret is deleted ONLY
// if iterion created it (securityReadCreatedBy) — an operator's hand-set
// secret is never destroyed, it keeps an explicit empty map instead (the
// bot's missing-org gate then fails loudly, which is the honest outcome).
// No-op when the secret or the entry is absent. Same CAS discipline as the
// upsert: a plain overwrite could resurrect a concurrent writer's entry.
func RemoveSecurityReadToken(ctx context.Context, st secrets.GenericSecretStore, sealer secrets.Sealer, conn *Connection) error {
	// Both the key the token was FILED under and the one derivable now: an org
	// rename makes them differ, and taking only the derived one strands a live
	// token nothing can ever reach. A missing derived key is not fatal when a
	// recorded one exists — withdrawal must work on exactly the connections
	// whose key can no longer be computed.
	keys := map[string]bool{}
	if recorded := strings.ToLower(strings.TrimSpace(conn.SecurityReadOrgKey)); recorded != "" {
		keys[recorded] = true
	}
	org, err := securityReadOrgKey(conn)
	if err != nil {
		if len(keys) == 0 {
			return err
		}
	} else {
		keys[org] = true
	}
	for attempt := 0; ; attempt++ {
		gs, ok, err := findSecurityReadSecret(ctx, st, conn.TenantID)
		if err != nil || !ok {
			return err
		}
		plain, err := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
		if err != nil {
			return fmt.Errorf("forge: open %s secret: %w", SecurityReadSecretName, err)
		}
		tokens, err := securityReadTokens(plain)
		if err != nil {
			return err
		}
		removed := false
		for k := range keys {
			if _, present := tokens[k]; present {
				delete(tokens, k)
				removed = true
			}
		}
		if !removed {
			return nil
		}
		if len(tokens) == 0 && gs.CreatedBy == securityReadCreatedBy {
			if err := st.Delete(ctx, gs.ID); err != nil {
				return fmt.Errorf("forge: delete emptied %s secret: %w", SecurityReadSecretName, err)
			}
			return nil
		}
		expected := gs.Fingerprint
		encoded, err := encodeSecurityReadTokens(tokens)
		if err != nil {
			return err
		}
		sealed, err := secrets.SealGenericSecret(sealer, gs.ID, encoded)
		if err != nil {
			return err
		}
		gs.SealedSecret = sealed
		gs.Fingerprint = secrets.FingerprintSHA256(string(encoded))
		err = guardedUpdate(ctx, st, gs, expected)
		if errors.Is(err, secrets.ErrGenericSecretConflict) && attempt == 0 {
			continue
		}
		if err != nil {
			return fmt.Errorf("forge: update %s secret: %w", SecurityReadSecretName, err)
		}
		return nil
	}
}
