package forge

import (
	"context"
	"encoding/json"
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
		return nil, fmt.Errorf("forge: %s secret is not a JSON map of org→token: %w", SecurityReadSecretName, err)
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
	org := strings.ToLower(strings.TrimSpace(conn.AccountLogin))
	if org == "" {
		return "", fmt.Errorf("forge: connection %s has no account login — cannot key its security-read token", conn.ID)
	}
	return org, nil
}

// UpsertSecurityReadToken merges {org_login: token} into the tenant's
// dependabot_tokens secret, creating it (team-scoped, egress-pinned to the
// forge host) on first use. Shared by the refresh worker's hourly re-mint
// and the enable endpoint's immediate mint, so both write the same shape.
func UpsertSecurityReadToken(ctx context.Context, st secrets.GenericSecretStore, sealer secrets.Sealer, conn *Connection, token, actor string, now time.Time) error {
	org, err := securityReadOrgKey(conn)
	if err != nil {
		return err
	}
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
			CreatedBy:    actor,
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
		return nil
	}
	sealed, err := secrets.SealGenericSecret(sealer, gs.ID, encoded)
	if err != nil {
		return err
	}
	gs.SealedSecret = sealed
	gs.Fingerprint = secrets.FingerprintSHA256(string(encoded))
	if err := st.Update(ctx, gs); err != nil {
		return fmt.Errorf("forge: update %s secret: %w", SecurityReadSecretName, err)
	}
	return nil
}

// RemoveSecurityReadToken drops the connection's org entry from the
// dependabot_tokens secret; when the map empties, the secret itself is
// deleted (a leftover empty map would read as "configured but broken" to
// the bot's explicit-error gate). No-op when the secret or the entry is
// absent.
func RemoveSecurityReadToken(ctx context.Context, st secrets.GenericSecretStore, sealer secrets.Sealer, conn *Connection) error {
	org, err := securityReadOrgKey(conn)
	if err != nil {
		return err
	}
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
	if _, present := tokens[org]; !present {
		return nil
	}
	delete(tokens, org)
	if len(tokens) == 0 {
		if err := st.Delete(ctx, gs.ID); err != nil {
			return fmt.Errorf("forge: delete emptied %s secret: %w", SecurityReadSecretName, err)
		}
		return nil
	}
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
	if err := st.Update(ctx, gs); err != nil {
		return fmt.Errorf("forge: update %s secret: %w", SecurityReadSecretName, err)
	}
	return nil
}
