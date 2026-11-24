package forge

import (
	"github.com/SocialGouv/iterion/pkg/connection"
)

// The forge connection MODEL — the record, its enums, its store and its
// credential sealer — lives in pkg/connection since the #2760 contract
// (ADR-098 decision 3: the forges become connectors #1-3). What remains in
// this package is the BEHAVIOUR built on that model: the connect flows, the
// refresh worker, the provisioning orchestrator, the three forge clients.
//
// These aliases keep the forge layer's vocabulary compiling unchanged while
// naming the owner: every identifier below IS the pkg/connection one, not a
// copy, so a value produced here is the value the store there persists. New
// code should import pkg/connection directly; the aliases exist so the
// contract could land as one reviewable move instead of a thousand-line
// rename across the whole tree.

type (
	// Connection is connection.ForgeConnection.
	Connection = connection.ForgeConnection
	// Provider is connection.Provider.
	Provider = connection.Provider
	// Kind is connection.Kind.
	Kind = connection.Kind
	// ConnectionStatus is connection.Status — one status vocabulary.
	ConnectionStatus = connection.Status
	// Purpose is connection.Purpose.
	Purpose = connection.Purpose
)

const (
	ProviderGitLab  = connection.ProviderGitLab
	ProviderGitHub  = connection.ProviderGitHub
	ProviderForgejo = connection.ProviderForgejo

	KindOAuthApp  = connection.KindOAuthApp
	KindGitHubApp = connection.KindGitHubApp
	KindPAT       = connection.KindPAT

	StatusActive      = connection.StatusActive
	StatusNeedsReauth = connection.StatusNeedsReauth
	StatusRevoked     = connection.StatusRevoked
	StatusDegraded    = connection.StatusDegraded

	PurposeRuntime      = connection.PurposeRuntime
	PurposeSecurityRead = connection.PurposeSecurityRead

	BotIdentityConnection   = connection.BotIdentityConnection
	BotIdentityProjectToken = connection.BotIdentityProjectToken

	AccountKindUser         = connection.AccountKindUser
	AccountKindBot          = connection.AccountKindBot
	AccountKindInstallation = connection.AccountKindInstallation
)

// ErrConnectionNotFound is connection.ErrForgeConnectionNotFound — one
// sentinel, so errors.Is answers the same on either spelling.
var ErrConnectionNotFound = connection.ErrForgeConnectionNotFound

// DefaultBaseURL is connection.DefaultBaseURL.
func DefaultBaseURL(p Provider) string { return connection.DefaultBaseURL(p) }

// CanonicalBaseURL is connection.CanonicalBaseURL.
func CanonicalBaseURL(p Provider, raw string) string {
	return connection.CanonicalBaseURL(p, raw)
}
