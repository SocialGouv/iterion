package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo/address"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mongoContextNamespace describes the configured destination, not the current
// SRV answer, selected replica or authentication identity. DNS aliases remain
// distinct declarations; matching aliases requires an operator migration.
type mongoContextNamespace struct {
	Scheme         string   `json:"scheme"`
	Hosts          []string `json:"hosts"`
	Database       string   `json:"database"`
	SRVServiceName string   `json:"srv_service,omitempty"`
	Tenant         string   `json:"tenant,omitempty"`
}

// configuredContextNamespace runs after mongo.Connect validates opts. Extract
// only the authority, remove userinfo, then let the native parser interpret
// its hosts using the direct scheme. A second SRV parse would query DNS again;
// opts.Hosts is already a possibly random SRV subset, not a stable identity.
func configuredContextNamespace(opts *options.ClientOptions, database string) (*mongoContextNamespace, error) {
	scheme, rest, ok := strings.Cut(opts.GetURI(), "://")
	if !ok || (scheme != "mongodb" && scheme != "mongodb+srv") {
		return nil, fmt.Errorf("store/mongo: identity requires a validated Mongo connection")
	}
	authority := rest
	// The validated native grammar consumes userinfo first. A literal '?'
	// there is valid, and an '@' in a later option is not the separator.
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	if end := strings.IndexAny(authority, "/?"); end >= 0 {
		authority = authority[:end]
	}
	parsed, err := connstring.Parse("mongodb://" + authority)
	if err != nil {
		// Do not propagate a parser diagnostic that could echo input.
		return nil, fmt.Errorf("store/mongo: cannot derive configured host identity")
	}
	ns := &mongoContextNamespace{Scheme: scheme, Database: database}
	for _, raw := range parsed.RawHosts {
		if address.Address(raw).Network() == "unix" {
			// The driver also accepts relative Unix sockets. Preserve case and
			// distinguish the same relative spelling in different directories.
			// Lexical cleaning changes the kernel destination of symlink/..
			// paths. Prefix relative paths without Join, Clean or Abs.
			path := raw
			// Linux abstract sockets are names, not filesystem paths. The
			// native transport accepts both @ and NUL as their first byte.
			abstract := runtime.GOOS == "linux" && (strings.HasPrefix(path, "@") || strings.HasPrefix(path, "\x00"))
			if !filepath.IsAbs(path) && !abstract {
				cwd, err := os.Getwd()
				if err != nil {
					return nil, fmt.Errorf("store/mongo: cannot resolve socket identity")
				}
				path = cwd + string(os.PathSeparator) + path
			}
			// JSON replaces invalid UTF-8; URL escaping preserves every path
			// byte before the namespace is serialized.
			ns.Hosts = append(ns.Hosts, "unix:"+url.PathEscape(path))
			continue
		}
		host, port, splitErr := net.SplitHostPort(raw)
		if splitErr != nil {
			host, port = strings.Trim(raw, "[]"), "27017"
		}
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
		} else if !strings.Contains(host, ":") {
			// A final dot disables the resolver's search domains.
			host = strings.ToLower(host)
		}
		if number, err := strconv.Atoi(port); err == nil {
			port = strconv.Itoa(number)
		}
		ns.Hosts = append(ns.Hosts, net.JoinHostPort(host, port))
	}
	slices.Sort(ns.Hosts)
	ns.Hosts = slices.Compact(ns.Hosts)
	if len(ns.Hosts) == 0 || database == "" {
		return nil, fmt.Errorf("store/mongo: identity requires hosts and database")
	}
	if scheme == "mongodb+srv" {
		ns.SRVServiceName = "mongodb"
		if opts.SRVServiceName != nil && *opts.SRVServiceName != "" {
			ns.SRVServiceName = *opts.SRVServiceName
		}
	}
	return ns, nil
}

// ContextIdentity includes the same tenant scope as Mongo's query filter.
// The owner attributes writes; it does not choose which runs the store sees.
func (s *Store) ContextIdentity(ctx context.Context) (store.ContextRef, error) {
	if s.contextNamespace == nil {
		return store.ContextRef{}, fmt.Errorf("store/mongo: store identity is not initialized")
	}
	ns := *s.contextNamespace
	ns.Tenant, _ = store.TenantFromContext(ctx)
	if ns.Tenant == "" && !store.IsWithoutTenantFilter(ctx) {
		return store.ContextRef{}, fmt.Errorf("store/mongo: identity requires a tenant or explicit unscoped context")
	}
	encoded, err := json.Marshal(ns)
	if err != nil {
		return store.ContextRef{}, fmt.Errorf("store/mongo: encode store identity: %w", err)
	}
	return store.NewStoreContextRef("mongodb", string(encoded)), nil
}
