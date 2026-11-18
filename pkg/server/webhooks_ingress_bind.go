package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The WRITE half of the connector ingress lane (ADR-168 §3, §6): binding an
// ingress records what the lane later holds every delivery to — the
// connection it is believed through and that connection's connector, the
// digest of the package's `ingress:` block as the operator consented to it,
// the installation the connection belongs to — and seals the secret the
// package's scheme verifies with, minted by iterion or pasted from the
// vendor as the package declares, recording that origin.

// Secret origins a package's credential declares (spec.IngressCredential).
const (
	ingressSecretMinted = "minted"
	ingressSecretVendor = "vendor"
)

// minVendorSecretLen is the shortest vendor secret an ingress accepts: every
// first-wave vendor's is longer (Slack's signing secret is 32 characters, a
// Mattermost token 26), and a shorter one is guessable.
const minVendorSecretLen = 16

// errIngressRequest marks a refusal the operator's request caused (400), as
// opposed to sealing failing (500); errIngressConsent one that asks the
// operator to consent to a contract (409).
var (
	errIngressRequest = errors.New("ingress request")
	errIngressConsent = errors.New("ingress consent")
)

// connectorBindingStatus is the status validateConnectorBinding's error
// answers on the legacy connector lane: an outage is 503, a verdict 400.
func connectorBindingStatus(err error) int {
	if errors.Is(err, errIngressStoreUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

// ingressWriteStatus is the status a write-half error answers.
func ingressWriteStatus(err error) int {
	switch {
	case errors.Is(err, errIngressStoreUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, errIngressConsent):
		return http.StatusConflict
	case errors.Is(err, errIngressRequest):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// ingressPackage resolves the package a connection of the team can bind,
// and the digest of its ingress contract as it resolves now. It validates
// the connection (validateConnectorBinding) and, when cfg is already bound,
// that the connection names cfg's connector: the sealed secret is that
// vendor's.
func (s *Server) ingressPackage(ctx context.Context, teamID string, cfg *webhooks.Config, connID string) (*spec.Package, string, string, error) {
	if connID == "" {
		return nil, "", "", fmt.Errorf("%w: a connector ingress needs connector_connection_id: the connection whose vendor it receives deliveries from", errIngressRequest)
	}
	conn, err := s.validateConnectorBinding(ctx, teamID, webhooks.ProviderConnector, connID)
	if errors.Is(err, errIngressStoreUnavailable) {
		return nil, "", "", err
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: %v", errIngressRequest, err)
	}
	if cfg.Connector != "" && conn.Connector != cfg.Connector {
		return nil, "", "", fmt.Errorf("%w: connection %q is a %q connection; this ingress receives %q deliveries and holds that vendor's secret — create a new ingress for %s",
			errIngressRequest, connID, conn.Connector, cfg.Connector, conn.Connector)
	}
	pkg, err := s.catalogPackage(teamID, conn.Connector, errIngressRequest)
	if err != nil {
		return nil, "", "", err
	}
	if pkg.Ingress == nil {
		return nil, "", "", fmt.Errorf("%w: the %s package declares no ingress (triggers.yaml `ingress:`): it receives no delivery", errIngressRequest, conn.Connector)
	}
	if len(pkg.IngressTriggers()) == 0 {
		// A polled trigger is fed by its poller, so a package whose only
		// triggers are polled would filter every delivery this ingress takes.
		return nil, "", "", fmt.Errorf("%w: the %s package declares no trigger a delivery can reach (a polled trigger is fed by its poller): every delivery would be filtered", errIngressRequest, conn.Connector)
	}
	digest, err := spec.IngressDigest(pkg.Ingress)
	if err != nil {
		return nil, "", "", err
	}
	return pkg, digest, conn.Connector, nil
}

// checkIngressInstallation requires the installation exactly when the
// package reads one.
func checkIngressInstallation(pkg *spec.Package, connector, installation string) error {
	switch {
	case pkg.Ingress.Installation != "" && installation == "":
		return fmt.Errorf("%w: the %s package reads each delivery's installation (%s): set installation to the value of the installation the connection belongs to",
			errIngressRequest, connector, pkg.Ingress.Installation)
	case pkg.Ingress.Installation == "" && installation != "":
		return fmt.Errorf("%w: the %s package reads no installation: an installation set here would never be checked", errIngressRequest, connector)
	}
	return nil
}

// checkIngressConsent holds a stated contract_digest to the contract the
// package resolves to now: the operator consents to the contract they read,
// not to whatever resolves when the request lands.
func checkIngressConsent(stated *string, current string) error {
	if stated != nil && strings.TrimSpace(*stated) != current {
		return fmt.Errorf("%w: contract_digest %q is not the package's contract, which resolves to %s — read it again and restate that digest",
			errIngressConsent, strings.TrimSpace(*stated), current)
	}
	return nil
}

// bindConnectorIngress validates and records a new ingress's binding to
// connID: the connection may be believed, its package declares an ingress
// and a trigger, the installation is set exactly when the package reads one,
// and a stated contract_digest is the one the package resolves to. Creating
// the ingress is the consent to that contract, which it pins. It returns the
// package, whose credential says where the secret comes from.
func (s *Server) bindConnectorIngress(ctx context.Context, teamID string, cfg *webhooks.Config, connID, installation string, consent *string) (*spec.Package, error) {
	pkg, digest, connector, err := s.ingressPackage(ctx, teamID, cfg, connID)
	if err != nil {
		return nil, err
	}
	installation = strings.TrimSpace(installation)
	if err := checkIngressInstallation(pkg, connector, installation); err != nil {
		return nil, err
	}
	if err := checkIngressConsent(consent, digest); err != nil {
		return nil, err
	}
	cfg.ConnectorConnectionID = connID
	cfg.Connector = connector
	cfg.ContractDigest = digest
	cfg.Installation = installation
	return pkg, nil
}

// rebindConnectorIngress is a PATCH of a connector ingress's binding: its
// connection, its installation, its consented contract. A restated value is
// no change, so a client that echoes the row back re-pins nothing. A real
// change re-validates against the package as it resolves now, and the pin
// moves only on the operator's explicit consent — contract_digest equal to
// the current contract — and only to a contract whose secret origin is the
// ingress's own: a contract that changes it needs a rotation first.
func (s *Server) rebindConnectorIngress(ctx context.Context, teamID string, cfg *webhooks.Config, connID, installation, consent *string) error {
	nextConn := cfg.ConnectorConnectionID
	if connID != nil {
		if *connID == "" {
			return fmt.Errorf("%w: a connector ingress is its binding and cannot be unbound — delete the ingress instead", errIngressRequest)
		}
		nextConn = *connID
	}
	nextInst := cfg.Installation
	if installation != nil {
		nextInst = strings.TrimSpace(*installation)
	}
	if nextConn == cfg.ConnectorConnectionID && nextInst == cfg.Installation && consent == nil {
		return nil
	}
	pkg, digest, connector, err := s.ingressPackage(ctx, teamID, cfg, nextConn)
	if err != nil {
		return err
	}
	if err := checkIngressInstallation(pkg, connector, nextInst); err != nil {
		return err
	}
	if err := checkIngressConsent(consent, digest); err != nil {
		return err
	}
	if digest != cfg.ContractDigest {
		if consent == nil {
			return fmt.Errorf("%w: the package's ingress contract changed since this ingress was bound (pinned %s, now %s) — restate contract_digest %s to consent to it",
				errIngressConsent, cfg.ContractDigest, digest, digest)
		}
		if origin := pkg.Ingress.Credential.Secret; origin != cfg.SecretOrigin {
			return fmt.Errorf("%w: the new contract's secret is %s, this ingress holds a %s one — rotate the ingress to re-seal its secret, then consent",
				errIngressConsent, origin, cfg.SecretOrigin)
		}
	}
	cfg.ConnectorConnectionID = nextConn
	cfg.Connector = connector
	cfg.ContractDigest = digest
	cfg.Installation = nextInst
	return nil
}

// sealIngressSecret seals the secret a new or rotated ingress verifies with,
// from the origin its package declares, and records that origin: the minted
// plaintext, or the vendor's secret the operator pasted. It returns what the
// answer shows the operator once — the minted secret, or nothing for a
// vendor's, which the operator already holds.
func (s *Server) sealIngressSecret(cfg *webhooks.Config, origin, minted string, vendorSecret *string) (string, error) {
	switch origin {
	case ingressSecretMinted:
		if vendorSecret != nil {
			return "", fmt.Errorf("%w: this package mints its ingress secret — iterion generates it and shows it once; vendor_secret is not accepted", errIngressRequest)
		}
		sealed, err := s.sealWebhookHMAC(cfg.ID, minted)
		if err != nil {
			return "", err
		}
		cfg.HMACSecretSealed = sealed
		cfg.SecretOrigin = origin
		return minted, nil
	case ingressSecretVendor:
		if vendorSecret == nil {
			return "", fmt.Errorf("%w: this package verifies with the vendor's secret — paste it as vendor_secret", errIngressRequest)
		}
		secret := strings.TrimSpace(*vendorSecret)
		if err := secrets.ValidateTokenShape("vendor_secret", secret); err != nil {
			return "", fmt.Errorf("%w: %v", errIngressRequest, err)
		}
		if len(secret) < minVendorSecretLen {
			return "", fmt.Errorf("%w: vendor_secret is %d characters; a vendor's signing secret or token is at least %d — paste the whole value",
				errIngressRequest, len(secret), minVendorSecretLen)
		}
		sealed, err := s.sealWebhookHMAC(cfg.ID, secret)
		if err != nil {
			return "", err
		}
		cfg.HMACSecretSealed = sealed
		cfg.SecretOrigin = origin
		// The minted token authenticates nothing on this ingress, and any
		// piece of the vendor's secret shown to the team's viewers would
		// shorten a guess at it: the row shows none.
		cfg.TokenHash, cfg.TokenLast4, cfg.Fingerprint = "", "", ""
		return "", nil
	default:
		return "", fmt.Errorf("%w: the package declares an unknown secret origin %q (minted|vendor)", errIngressRequest, origin)
	}
}

// catalogPackage resolves a connector through the team's catalog for the
// write half: a verdict about the catalog (no such connector, a package it
// holds and cannot load) is the caller's kind of refusal, an unreadable
// store is an outage whose cause stays in the log.
func (s *Server) catalogPackage(teamID, connector string, verdict error) (*spec.Package, error) {
	pkg, _, err := s.connectorCatalogFor(teamID).Package(connector)
	switch {
	case err == nil:
		return pkg, nil
	case connection.IsCatalogUnavailable(err):
		if s.logger != nil {
			s.logger.Warn("webhooks: resolve connector %s for team %s: %v", connector, teamID, err)
		}
		return nil, fmt.Errorf("%w: the connector catalog is unavailable; retry", errIngressStoreUnavailable)
	case connection.IsNoSuchConnector(err):
		return nil, fmt.Errorf("%w: the team's catalog holds no connector %q", verdict, connector)
	}
	return nil, fmt.Errorf("%w: the team's catalog holds connector %q but cannot load it: %v", verdict, connector, err)
}

// ingressSecretOrigin reads the secret origin the ingress's package
// declares, for a rotation — from the connector the ingress recorded, so
// neither a revoked nor a deleted connection blocks it: rotating a leaked
// secret is incident response, and re-sealing from a new origin is the step
// a consent may wait on.
func (s *Server) ingressSecretOrigin(cfg webhooks.Config) (string, error) {
	if s.connectorCatalog == nil {
		return "", fmt.Errorf("%w: a connector ingress needs the connection plane, which this deployment does not wire", errIngressConsent)
	}
	if cfg.Connector == "" {
		return "", fmt.Errorf("%w: this ingress records no connector — re-bind it first", errIngressConsent)
	}
	pkg, err := s.catalogPackage(cfg.TenantID, cfg.Connector, errIngressConsent)
	if err != nil {
		return "", err
	}
	if pkg.Ingress == nil {
		return "", fmt.Errorf("%w: the %s package declares no ingress", errIngressConsent, cfg.Connector)
	}
	return pkg.Ingress.Credential.Secret, nil
}
