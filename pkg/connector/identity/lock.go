// Package identity preserves the meaning of connector names across generation.
// Its lock is generation input, not an extra runtime catalog or queue format.
package identity

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

const File = "identity.lock.yaml"
const SchemaVersion = 1

type Operation struct {
	GeneratedID string   `yaml:"generated_id"`
	PublicIDs   []string `yaml:"public_ids,omitempty"`
}

// AuthShape excludes vendor labels and advertised/default scopes: those can
// change without changing where or how an existing credential is presented.
type AuthShape struct {
	Kind        spec.AuthKind `yaml:"kind"`
	In          string        `yaml:"in,omitempty"`
	Name        string        `yaml:"name,omitempty"`
	ValuePrefix string        `yaml:"value_prefix,omitempty"`
	AuthURL     string        `yaml:"auth_url,omitempty"`
	TokenURL    string        `yaml:"token_url,omitempty"`
	RevokeURL   string        `yaml:"revoke_url,omitempty"`
	PKCE        bool          `yaml:"pkce,omitempty"`
}

type Auth struct {
	ID    string    `yaml:"id"`
	Shape AuthShape `yaml:"shape"`
}

// Entries are retained when the vendor removes an operation or auth scheme.
// Reusing a retired name would change the meaning of an unchanged workflow.
type Lock struct {
	SchemaVersion int                  `yaml:"schema_version"`
	Connector     string               `yaml:"connector"`
	Operations    map[string]Operation `yaml:"operations"`
	Auth          map[string]Auth      `yaml:"generated_auth"`
	PublicAuth    map[string]AuthShape `yaml:"public_auth"`
}

func New(connector string) *Lock {
	return &Lock{SchemaVersion: SchemaVersion, Connector: connector, Operations: map[string]Operation{}, Auth: map[string]Auth{}, PublicAuth: map[string]AuthShape{}}
}

func Load(dir string) (*Lock, error) {
	data, err := os.ReadFile(filepath.Join(dir, File))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Lock, error) {
	var probe struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("identity lock: %w", err)
	}
	if probe.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("identity lock: schema_version %d is newer than supported %d (upgrade iterion)", probe.SchemaVersion, SchemaVersion)
	}
	var lock Lock
	if err := yaml.UnmarshalStrict(data, &lock); err != nil {
		return nil, fmt.Errorf("identity lock: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	return &lock, nil
}

func (l *Lock) Write(dir string) error {
	if err := l.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, File), data, 0o644)
}

func operationKey(op spec.Operation) string {
	return strings.ToUpper(op.HTTP.Method) + " " + op.HTTP.Path
}

func shape(s spec.AuthScheme) AuthShape {
	name := s.Name
	if s.In == "header" {
		name = strings.ToLower(name)
	}
	return AuthShape{s.Kind, s.In, name, s.ValuePrefix, s.AuthURL, s.TokenURL, s.RevokeURL, s.PKCE}
}

func authKey(s AuthShape) string {
	data, _ := json.Marshal(s) // fixed string/bool struct; encoding cannot fail
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (l *Lock) Validate() error {
	if l.SchemaVersion != SchemaVersion || l.Connector == "" || l.Operations == nil || l.Auth == nil || l.PublicAuth == nil {
		return fmt.Errorf("identity lock: incomplete or unsupported schema %d", l.SchemaVersion)
	}
	generated, public := map[string]string{}, map[string]string{}
	for key, op := range l.Operations {
		method, path, ok := strings.Cut(key, " ")
		if !ok || method == "" || method != strings.ToUpper(method) || !strings.HasPrefix(path, "/") {
			return fmt.Errorf("identity lock: invalid method/path %q", key)
		}
		if !strings.HasPrefix(op.GeneratedID, l.Connector+".") {
			return fmt.Errorf("identity lock: operation %q has invalid generated id %q", key, op.GeneratedID)
		}
		if err := reserve(generated, op.GeneratedID, key); err != nil {
			return err
		}
		for _, id := range op.PublicIDs {
			if !strings.HasPrefix(id, l.Connector+".") {
				return fmt.Errorf("identity lock: invalid public id %q", id)
			}
			if err := reserve(public, id, key); err != nil {
				return err
			}
		}
	}
	authIDs := map[string]string{}
	for key, entry := range l.Auth {
		if key != authKey(entry.Shape) || entry.ID == "" || entry.Shape.Kind == "" {
			return fmt.Errorf("identity lock: invalid auth identity %q", key)
		}
		if err := reserve(authIDs, entry.ID, key); err != nil {
			return err
		}
	}
	for id, s := range l.PublicAuth {
		if id == "" || s.Kind == "" {
			return fmt.Errorf("identity lock: invalid public auth id %q", id)
		}
	}
	return nil
}

func reserve(owners map[string]string, id, key string) error {
	if old, ok := owners[id]; ok && old != key {
		return fmt.Errorf("identity lock: %q belongs to %s, cannot reassign it to %s", id, old, key)
	}
	owners[id] = key
	return nil
}

func allocate(proposed, key string, owners map[string]string) string {
	if old, ok := owners[proposed]; !ok || old == key {
		return proposed
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	for size := 8; size <= len(digest); size += 8 {
		candidate := proposed + "_" + digest[:size]
		if old, ok := owners[candidate]; !ok || old == key {
			return candidate
		}
	}
	return "" // all digest prefixes already occupied; refuse rather than reassign
}

// Reconcile changes only the generated identifiers and their internal references.
// Existing entries win over vendor labels, operation order and removed entries.
// The caller works on an unpublished package and discards it on any error.
func (l *Lock) Reconcile(p *spec.Package) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if l.Connector != p.Connector.ID {
		return fmt.Errorf("identity lock: connector %q differs from %q", l.Connector, p.Connector.ID)
	}
	owners := map[string]string{}
	for key, entry := range l.Operations {
		owners[entry.GeneratedID] = key
	}
	for _, op := range p.Operations() {
		key := operationKey(op)
		if _, exists := l.Operations[key]; !exists {
			id := allocate(op.ID, key, owners)
			if id == "" {
				return fmt.Errorf("identity lock: cannot allocate %s", key)
			}
			l.Operations[key] = Operation{GeneratedID: id}
			owners[id] = key
		}
	}
	for i := range p.Ops {
		for j := range p.Ops[i].Operations {
			op := &p.Ops[i].Operations[j]
			op.ID = l.Operations[operationKey(*op)].GeneratedID
			parts := strings.SplitN(op.ID, ".", 3)
			if len(parts) != 3 {
				return fmt.Errorf("identity lock: invalid operation id %q", op.ID)
			}
			op.Resource, op.Verb = parts[1], parts[2]
		}
		slices.SortFunc(p.Ops[i].Operations, func(a, b spec.Operation) int { return strings.Compare(a.ID, b.ID) })
	}
	authOwners, remap, seen := map[string]string{}, map[string]string{}, map[string]bool{}
	for key, entry := range l.Auth {
		authOwners[entry.ID] = key
	}
	for i := range p.Connector.Auth {
		s := &p.Connector.Auth[i]
		fingerprint := shape(*s)
		key := authKey(fingerprint)
		if seen[key] {
			return fmt.Errorf("identity lock: ambiguous duplicate auth shape for %q", s.ID)
		}
		seen[key] = true
		entry, ok := l.Auth[key]
		if !ok {
			id := allocate(s.ID, key, authOwners)
			if id == "" {
				return fmt.Errorf("identity lock: cannot allocate auth %q", s.ID)
			}
			entry = Auth{ID: id, Shape: fingerprint}
			l.Auth[key], authOwners[id] = entry, key
		}
		remap[s.ID], s.ID = entry.ID, entry.ID
	}
	remapSecurity(p.Connector.DefaultSecurity, remap)
	for i := range p.Ops {
		for j := range p.Ops[i].Operations {
			remapSecurity(p.Ops[i].Operations[j].Security, remap)
		}
	}
	return p.ValidateGenerated()
}

func remapSecurity(reqs []spec.SecurityRequirement, names map[string]string) {
	for i := range reqs {
		for j := range reqs[i].Terms {
			if id, ok := names[reqs[i].Terms[j].SchemeID]; ok {
				reqs[i].Terms[j].SchemeID = id
			}
		}
	}
}

// ObservePublic permits an author's explicit new name while retaining all old
// names for that same HTTP identity. An overlay may never transfer a name to
// another operation, or move an existing auth id to another placement/issuer.
func (l *Lock) ObservePublic(p *spec.Package) error {
	owners := map[string]string{}
	for key, entry := range l.Operations {
		for _, id := range entry.PublicIDs {
			owners[id] = key
		}
	}
	for _, op := range p.Operations() {
		key := operationKey(op)
		entry, ok := l.Operations[key]
		if !ok {
			return fmt.Errorf("identity lock: public operation %s has no generated identity", key)
		}
		if err := reserve(owners, op.ID, key); err != nil {
			return err
		}
		if !slices.Contains(entry.PublicIDs, op.ID) {
			entry.PublicIDs = append(entry.PublicIDs, op.ID)
			slices.Sort(entry.PublicIDs)
		}
		l.Operations[key] = entry
	}
	for _, s := range p.Connector.Auth {
		fingerprint := shape(s)
		if old, ok := l.PublicAuth[s.ID]; ok && old != fingerprint {
			return fmt.Errorf("identity lock: public auth %q changed placement or issuer; use a new id", s.ID)
		}
		l.PublicAuth[s.ID] = fingerprint
	}
	return l.Validate()
}
