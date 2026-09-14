package natsconfig

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nats-io/nkeys"
)

// Profile is the credential-free projection of the supported static NATS
// 2.14.5 authorization configuration. It does not prove that the broker has
// loaded this digest, that CLI flags did not override authentication, or that
// every holder of these credentials has been inventoried.
type Profile struct {
	ParserVersion string      `json:"parser_version"`
	ConfigDigest  string      `json:"config_digest"`
	SystemAccount string      `json:"system_account"`
	Accounts      []string    `json:"accounts"`
	Principals    []Principal `json:"principals"`
}

type Principal struct {
	Account   string             `json:"account"`
	Identity  string             `json:"identity"`
	Kind      string             `json:"kind"`
	Publish   SubjectPermissions `json:"publish"`
	Subscribe SubjectPermissions `json:"subscribe"`
}

type effectivePermissions struct {
	publish   SubjectPermissions
	subscribe SubjectPermissions
}

// LoadProfile accepts only a closed, static configuration language. A caller
// must keep Result.Config private: it still contains password material. Local
// variables are allowed only at top level with a VAR_ name, so none can be
// mistaken for an operational or authorization key in the pinned server.
func LoadProfile(result *Result) (*Profile, error) {
	if result == nil || result.ParserVersion != ParserVersion || len(result.Digest) != 71 ||
		!strings.HasPrefix(result.Digest, "sha256:") || result.Config == nil {
		return nil, fmt.Errorf("NATS authority profile requires the pinned parsed configuration")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(result.Digest, "sha256:")); err != nil {
		return nil, fmt.Errorf("NATS authority profile has an invalid configuration digest")
	}
	variables := make(map[string]bool, len(result.Variables))
	for _, name := range result.Variables {
		if !profileVariableName(name) || variables[name] {
			return nil, fmt.Errorf("NATS authority profile uses an unsupported local variable name")
		}
		if _, ok := result.Config[name]; !ok {
			return nil, fmt.Errorf("NATS authority profile lost a local variable")
		}
		variables[name] = true
	}
	options := make(map[string]any)
	for name, raw := range result.Config {
		if variables[name] {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("NATS authority profile has ambiguous option aliases")
		}
		if !profileTopLevelKey(key) {
			return nil, fmt.Errorf("NATS authority profile contains an unsupported top-level option")
		}
		value, err := profileJSON(raw)
		if err != nil {
			return nil, err
		}
		options[key] = value
	}
	for _, key := range []string{"listen", "host", "server_name", "http", "store_dir"} {
		if value, ok := options[key]; ok {
			if s, ok := value.(string); !ok || s == "" {
				return nil, fmt.Errorf("NATS authority profile has an invalid operational option")
			}
		}
	}
	if value, ok := options["port"]; ok {
		if _, ok := value.(json.Number); !ok {
			return nil, fmt.Errorf("NATS authority profile has an invalid listener port")
		}
	}
	if value, ok := options["jetstream"]; ok {
		if err := profileJetStream(value); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("NATS authority profile requires explicit JetStream configuration")
	}
	system, ok := options["system_account"].(string)
	if !ok || !profileAccountName(system) {
		return nil, fmt.Errorf("NATS authority profile requires a named system account")
	}
	accounts, ok := options["accounts"].(map[string]any)
	if !ok || len(accounts) < 2 || len(accounts) > 32 {
		return nil, fmt.Errorf("NATS authority profile requires a bounded account map")
	}
	profile := &Profile{ParserVersion: ParserVersion, ConfigDigest: result.Digest, SystemAccount: system}
	identities := make(map[string]bool)
	for account, value := range accounts {
		if !profileAccountName(account) {
			return nil, fmt.Errorf("NATS authority profile has an unsupported account name")
		}
		fields, err := profileMap(value, "users", "default_permissions", "jetstream")
		if err != nil {
			return nil, err
		}
		if enabled, exists := fields["jetstream"]; exists {
			if _, ok := enabled.(bool); !ok {
				return nil, fmt.Errorf("NATS authority profile has unsupported account JetStream limits")
			}
		}
		defaults := effectivePermissions{}
		if source, exists := fields["default_permissions"]; exists {
			defaults, err = profilePermissions(source)
			if err != nil {
				return nil, err
			}
		}
		users, ok := fields["users"].([]any)
		if !ok || len(users) == 0 || len(users) > 128 || len(profile.Principals)+len(users) > 128 {
			return nil, fmt.Errorf("NATS authority profile requires bounded named users")
		}
		for _, user := range users {
			principal, err := profilePrincipal(account, user, defaults)
			if err != nil {
				return nil, err
			}
			if identities[principal.Identity] {
				return nil, fmt.Errorf("NATS authority profile has an ambiguous principal identity")
			}
			identities[principal.Identity] = true
			profile.Principals = append(profile.Principals, principal)
		}
		profile.Accounts = append(profile.Accounts, account)
	}
	if !slices.Contains(profile.Accounts, system) {
		return nil, fmt.Errorf("NATS authority profile system account is absent")
	}
	slices.Sort(profile.Accounts)
	slices.SortFunc(profile.Principals, func(a, b Principal) int {
		if c := strings.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		return strings.Compare(a.Identity, b.Identity)
	})
	return profile, nil
}

func profileVariableName(name string) bool {
	if !strings.HasPrefix(name, "VAR_") || len(name) < 5 || len(name) > 52 {
		return false
	}
	for _, c := range name[4:] {
		if !profileVariableRune(c) {
			return false
		}
	}
	return true
}

func profileTopLevelKey(key string) bool {
	switch key {
	case "listen", "host", "port", "server_name", "http", "store_dir", "jetstream", "system_account", "accounts":
		return true
	default:
		return false
	}
}

func profileAccountName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !profileAccountRune(c) {
			return false
		}
	}
	return true
}

func profileVariableRune(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func profileAccountRune(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func profileJSON(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("NATS authority profile has malformed parsed JSON")
	}
	return value, nil
}

func profileMap(source any, allowed ...string) (map[string]any, error) {
	input, ok := source.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("NATS authority profile requires a static object")
	}
	fields := make(map[string]any, len(input))
	for key, value := range input {
		lower := strings.ToLower(key)
		if !slices.Contains(allowed, lower) {
			return nil, fmt.Errorf("NATS authority profile contains an unsupported nested option")
		}
		if _, duplicate := fields[lower]; duplicate {
			return nil, fmt.Errorf("NATS authority profile has ambiguous nested option aliases")
		}
		fields[lower] = value
	}
	return fields, nil
}

func profileJetStream(source any) error {
	switch value := source.(type) {
	case bool:
		if value {
			return nil
		}
	case string:
		if strings.EqualFold(value, "enabled") || strings.EqualFold(value, "enable") {
			return nil
		}
	case map[string]any:
		fields, err := profileMap(value, "store_dir", "max_mem_store", "max_file_store", "enabled")
		if err != nil {
			return err
		}
		for key, field := range fields {
			switch key {
			case "store_dir":
				if _, ok := field.(string); !ok {
					return fmt.Errorf("NATS authority profile has invalid JetStream storage")
				}
			case "enabled":
				if field != true {
					return fmt.Errorf("NATS authority profile requires enabled JetStream")
				}
			default:
				if _, ok := field.(json.Number); !ok {
					if _, ok := field.(string); !ok {
						return fmt.Errorf("NATS authority profile has invalid JetStream storage limits")
					}
				}
			}
		}
		return nil
	}
	return fmt.Errorf("NATS authority profile requires enabled JetStream")
}

func profilePrincipal(account string, source any, defaults effectivePermissions) (Principal, error) {
	fields, err := profileMap(source, "user", "username", "pass", "password", "nkey", "permissions")
	if err != nil {
		return Principal{}, err
	}
	name, nameSet, err := profileStringField(fields, "user")
	if err != nil {
		return Principal{}, err
	}
	alias, aliasSet, err := profileStringField(fields, "username")
	if err != nil {
		return Principal{}, err
	}
	if aliasSet {
		name = alias
	}
	password, passwordSet, err := profileStringField(fields, "password")
	if err != nil {
		return Principal{}, err
	}
	pass, passSet, err := profileStringField(fields, "pass")
	if err != nil {
		return Principal{}, err
	}
	if passSet {
		password = pass
	}
	nkey, nkeySet, err := profileStringField(fields, "nkey")
	if err != nil {
		return Principal{}, err
	}
	principal := Principal{Account: account, Publish: defaults.publish, Subscribe: defaults.subscribe}
	switch {
	case nkeySet && !nameSet && !aliasSet && !passwordSet && !passSet && nkeys.IsValidPublicUserKey(nkey):
		principal.Identity, principal.Kind = nkey, "nkey"
	case !nkeySet && (nameSet != aliasSet) && (passwordSet != passSet) && profileAccountName(name) && password != "":
		principal.Identity, principal.Kind = name, "password"
	default:
		return Principal{}, fmt.Errorf("NATS authority profile requires one named password or public nkey identity")
	}
	if source, explicit := fields["permissions"]; explicit {
		permissions, err := profilePermissions(source)
		if err != nil {
			return Principal{}, err
		}
		principal.Publish, principal.Subscribe = permissions.publish, permissions.subscribe
	}
	return principal, nil
}

func profileStringField(fields map[string]any, key string) (string, bool, error) {
	value, exists := fields[key]
	if !exists {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, fmt.Errorf("NATS authority profile requires a string identity field")
	}
	return text, true, nil
}

func profilePermissions(source any) (effectivePermissions, error) {
	fields, err := profileMap(source, "publish", "subscribe")
	if err != nil {
		return effectivePermissions{}, err
	}
	var permissions effectivePermissions
	if value, ok := fields["publish"]; ok {
		permissions.publish, err = profileSubjectPermissions(value)
		if err != nil {
			return effectivePermissions{}, err
		}
	}
	if value, ok := fields["subscribe"]; ok {
		permissions.subscribe, err = profileSubjectPermissions(value)
		if err != nil {
			return effectivePermissions{}, err
		}
	}
	return permissions, nil
}

func profileSubjectPermissions(source any) (SubjectPermissions, error) {
	var result SubjectPermissions
	if object, ok := source.(map[string]any); ok {
		fields, err := profileMap(object, "allow", "deny")
		if err != nil {
			return result, err
		}
		if value, ok := fields["allow"]; ok {
			result.Allow, err = profileSubjects(value)
			if err != nil {
				return result, err
			}
		}
		if value, ok := fields["deny"]; ok {
			result.Deny, err = profileSubjects(value)
			if err != nil {
				return result, err
			}
		}
		return result, nil
	}
	var err error
	result.Allow, err = profileSubjects(source)
	return result, err
}

func profileSubjects(source any) ([]string, error) {
	var subjects []string
	switch value := source.(type) {
	case string:
		subjects = []string{value}
	case []any:
		if len(value) > 128 {
			return nil, fmt.Errorf("NATS authority profile exceeds supported permission count")
		}
		for _, item := range value {
			subject, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("NATS authority profile requires static subject permissions")
			}
			subjects = append(subjects, subject)
		}
	default:
		return nil, fmt.Errorf("NATS authority profile requires static subject permissions")
	}
	for _, subject := range subjects {
		if _, err := parseSubjectPattern(subject); err != nil {
			return nil, err
		}
	}
	return subjects, nil
}
