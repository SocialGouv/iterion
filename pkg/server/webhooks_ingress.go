package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The connector ingress lane (ADR-168 §3, §8): one inbound delivery for a
// connector package's ingress, read the way the package declares it and
// written to the durable intake before it is answered. The lane holds no
// vendor's name — the package's `ingress:` block says how the delivery is
// verified and read — and launches nothing: matching and launching run after
// the answer, from the intake.
//
// Its contract, in order:
//  1. the ingress resolves — an unknown id costs one read and answers 401,
//     like webhookAuth — and, before the sender is proven, only a
//     per-(client address, ingress) limit applies: nothing of the tenant's
//     is charged, junk aimed at other ids from a vendor's shared address does
//     not starve this ingress, and an id the sender invents creates no
//     limiter key (the limiter's keys are shared with login);
//  2. its body is read under the cap;
//  3. the binding can vouch: its connection exists in the ingress's tenant,
//     names the connector the ingress was bound for, holds the `trigger`
//     grant and is live, and its package resolves to the contract the
//     operator consented to (the pin) — refused otherwise (409), with nothing
//     written, a package the catalog holds but cannot load included; a store
//     that cannot answer is 503, never a binding verdict;
//  4. the package's reader authenticates the delivery (ingress.Read): a
//     delivery that fails answers 401 and changes no durable state;
//  5. admission, authenticated from here: the ingress is live, its rate
//     bucket allows, its tenant may launch;
//  6. a handshake is answered, a filtered delivery acknowledged — neither
//     writes anything;
//  7. a redelivery meets its row and is acknowledged; otherwise the backlog
//     cap and then the monthly quota — the one admission step that writes —
//     admit the delivery, and its intake row is written — detached from the
//     request, so a vendor hanging up does not cancel it halfway; a write
//     proven absent, or one that meets a row a concurrent twin wrote first,
//     gives the quota unit back;
//  8. the package's declared acknowledgement answers, and only an answered
//     delivery marks the ingress used.

// connectorIngressIPBucket bounds the deliveries one client address may make
// the lane verify for one existing ingress: before verification it is the
// only limit (ADR-168 §3). Keyed by the resolved ingress as well, because a
// vendor delivers every tenant's hooks from one shared pool of addresses.
var connectorIngressIPBucket = authBucketCfg{rate: 20, burst: 200}

// connectorBacklogRetryAfter is the pause a vendor is asked to take when the
// tenant's backlog is full.
const connectorBacklogRetryAfter = 30 * time.Second

// ingressWriteTimeout bounds the intake write, which runs detached from the
// request.
const ingressWriteTimeout = 10 * time.Second

// maxIngressErrorText bounds what a refusal's cause may put in the log or an
// answer: a cause can quote the delivery, and the delivery is the sender's.
const maxIngressErrorText = 512

func (s *Server) registerConnectorIngressRoute() {
	s.mux.HandleFunc("POST /api/webhooks/c/{id}", s.handleConnectorIngress)
}

// ingressBinding is what an ingress is believed through: its connection and
// the package that connection's connector resolves to for the tenant.
type ingressBinding struct {
	conn connection.Connection
	pkg  *spec.Package
}

// ingressBindingError is a binding that cannot vouch for a delivery. It
// names what the operator fixes, so it is the answer's text as well as the
// log's.
type ingressBindingError struct {
	status int
	reason string
}

func (e *ingressBindingError) Error() string { return e.reason }

// errIngressStoreUnavailable is a store that could not answer while the
// binding was resolved: a retry may succeed, and the cause stays in the log.
var errIngressStoreUnavailable = errors.New("a store the ingress resolves through is unavailable")

// ContractChangedError refuses a delivery whose package now resolves to a
// contract other than the one the ingress was bound under.
type ContractChangedError struct {
	IngressID string
	Pinned    string
	Current   string
}

func (e *ContractChangedError) Error() string {
	return fmt.Sprintf("ingress %s was bound under the package's ingress contract %s; the package now resolves to %s — consent to it by restating contract_digest %s on the ingress",
		e.IngressID, e.Pinned, e.Current, e.Current)
}

// resolveIngressBinding resolves cfg's binding and checks it can vouch for
// a delivery: the connection, read tenant-scoped, names the ingress's
// connector, holds the trigger grant and is live, and the package resolves
// to the pinned contract. The same checks guard the write
// (bindConnectorIngress); these are for the record that changed since.
func (s *Server) resolveIngressBinding(ctx context.Context, cfg webhooks.Config) (ingressBinding, error) {
	if s.connectionStore == nil || s.connectorCatalog == nil {
		return ingressBinding{}, &ingressBindingError{http.StatusServiceUnavailable,
			"this deployment has no connection plane to resolve a connector ingress"}
	}
	conn, err := s.connectionStore.Get(ctx, cfg.TenantID, cfg.ConnectorConnectionID)
	switch {
	case errors.Is(err, connection.ErrNotFound):
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the connection this ingress is bound to (%s) no longer exists — re-bind or recreate it", cfg.ConnectorConnectionID)}
	case err != nil:
		return ingressBinding{}, fmt.Errorf("%w: read connection %s: %v", errIngressStoreUnavailable, cfg.ConnectorConnectionID, err)
	}
	if cfg.Connector == "" || conn.Connector != cfg.Connector {
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the bound connection %q is a %q connection; this ingress receives %q deliveries — re-bind it to a %s connection", conn.Alias, conn.Connector, cfg.Connector, cfg.Connector)}
	}
	if !conn.Has(connection.CapTrigger) {
		capErr := &connection.CapabilityError{Alias: conn.Alias, Want: connection.CapTrigger, Have: conn.Capabilities}
		return ingressBinding{}, &ingressBindingError{http.StatusConflict, capErr.Error()}
	}
	if !conn.Status.Usable() {
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the bound connection %q (%s) is %s — a revoked or reauth-pending connection vouches for no delivery; reconnect or re-bind", conn.Alias, conn.ID, conn.Status)}
	}
	pkg, _, err := s.connectorCatalogFor(cfg.TenantID).Package(conn.Connector)
	switch {
	case connection.IsNoSuchConnector(err):
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the team's catalog no longer holds the bound connector %q", conn.Connector)}
	case connection.IsCatalogUnavailable(err):
		return ingressBinding{}, fmt.Errorf("%w: resolve connector %s: %v", errIngressStoreUnavailable, conn.Connector, err)
	case err != nil:
		// The catalog holds the package and cannot load it — a broken
		// override waits for a repair, not for a retry. The cause stays in
		// the log: the answer goes to whoever holds the URL.
		if s.logger != nil {
			s.logger.Warn("webhooks: connector ingress %s: connector %s does not load: %s", cfg.ID, conn.Connector, boundedText(err.Error(), maxIngressErrorText))
		}
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the team's catalog holds connector %q but cannot load it — repair or republish the package", conn.Connector)}
	}
	if pkg.Ingress == nil {
		return ingressBinding{}, &ingressBindingError{http.StatusConflict,
			fmt.Sprintf("the %s package declares no ingress — it receives no delivery", conn.Connector)}
	}
	current, err := spec.IngressDigest(pkg.Ingress)
	if err != nil {
		return ingressBinding{}, err
	}
	if current != cfg.ContractDigest {
		return ingressBinding{}, &ContractChangedError{IngressID: cfg.ID, Pinned: cfg.ContractDigest, Current: current}
	}
	return ingressBinding{conn: conn, pkg: pkg}, nil
}

// handleConnectorIngress is the lane (see the contract above).
func (s *Server) handleConnectorIngress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcIP := s.clientIP(r)
	cfg, err := s.webhookConfigs.Get(ctx, r.PathValue("id"))
	if err != nil && !errors.Is(err, webhooks.ErrNotFound) {
		s.webhookConfigUnavailable(w, webhooks.ProviderConnector, err)
		return
	}
	if err != nil || cfg.Provider != webhooks.ProviderConnector {
		// Not found and another provider's id answer alike, so the id space
		// cannot be probed.
		httpError(w, http.StatusUnauthorized, "invalid webhook")
		return
	}
	if s.authLimiter != nil {
		if ok, retry := s.authLimiter.allow("wcip:"+s.rateBucket(r)+"|"+cfg.ID, connectorIngressIPBucket); !ok {
			w.Header().Set("Retry-After", retrySeconds(retry))
			httpError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		httpError(w, http.StatusRequestEntityTooLarge, "the body exceeds the %d-byte cap", maxWebhookBodyBytes)
		return
	}

	b, err := s.resolveIngressBinding(ctx, cfg)
	if err != nil {
		s.warnIngress(cfg, srcIP, "binding refused", err)
		var be *ingressBindingError
		var ce *ContractChangedError
		switch {
		case errors.As(err, &be):
			httpError(w, be.status, "%s", be.reason)
		case errors.As(err, &ce):
			httpError(w, http.StatusConflict, "%s", ce.Error())
		case errors.Is(err, errIngressStoreUnavailable):
			httpError(w, http.StatusServiceUnavailable, "the ingress cannot be resolved right now; retry")
		default:
			httpError(w, http.StatusInternalServerError, "the ingress binding could not be resolved")
		}
		return
	}
	secret, err := webhooks.OpenHMACSecret(s.sealer, cfg.ID, cfg.HMACSecretSealed)
	if err != nil {
		s.warnIngress(cfg, srcIP, "secret unavailable", err)
		httpError(w, http.StatusInternalServerError, "the ingress's secret is unavailable")
		return
	}
	now := s.ingressNow()
	d, err := ingress.Read(ingress.Request{
		Ingress:      b.pkg.Ingress,
		Triggers:     b.pkg.Triggers,
		Secret:       secret,
		Installation: cfg.Installation,
		Body:         body,
		Header:       r.Header,
		Query:        r.URL.Query(),
		Now:          now,
	})
	switch {
	case errors.Is(err, ingress.ErrUnauthenticated):
		// Why stays in the log: the answer must not teach a forger which
		// part of the proof failed, nor echo the bound installation.
		s.warnIngress(cfg, srcIP, "delivery not authenticated", err)
		httpError(w, http.StatusUnauthorized, "delivery not authenticated")
		return
	case errors.Is(err, ingress.ErrEventTooLarge):
		s.warnIngress(cfg, srcIP, "event too large", err)
		httpError(w, http.StatusRequestEntityTooLarge, "%s", boundedText(err.Error(), maxIngressErrorText))
		return
	case err != nil:
		s.warnIngress(cfg, srcIP, "malformed delivery", err)
		httpError(w, http.StatusBadRequest, "%s", boundedText(err.Error(), maxIngressErrorText))
		return
	}

	// Authenticated: the admission every inbound lane applies.
	if !s.webhookLive(w, r, cfg) || !s.webhookRateAllowed(w, cfg) {
		return
	}
	if _, ok := s.webhookTenantActive(w, r, cfg); !ok {
		return
	}

	if d.Handshake != nil {
		s.markWebhookUsed(cfg)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, *d.Handshake)
		return
	}
	if d.Filtered != "" {
		if s.logger != nil {
			s.logger.Info("webhooks: connector ingress %s filtered a delivery: %s", cfg.ID, d.Filtered)
		}
		s.markWebhookUsed(cfg)
		writeIngressAck(w, d.Ack)
		return
	}

	// A redelivery meets the row its first delivery opened, and is answered
	// as that one was — before the cap and the quota, which it does not
	// spend again.
	intakeID := intake.IntakeID(cfg.TenantID, intake.IngressSource(cfg.ID), d.DedupKey)
	if _, err := s.connectorIntakes.GetIntake(ctx, cfg.TenantID, intakeID); err == nil {
		s.markWebhookUsed(cfg)
		writeIngressAck(w, d.Ack)
		return
	} else if !errors.Is(err, intake.ErrNotFound) {
		s.warnIngress(cfg, srcIP, "intake lookup failed", err)
		httpError(w, http.StatusServiceUnavailable, "the intake store is unavailable")
		return
	}
	// The cap is read, then written past: concurrent deliveries may each
	// see room, so it bounds the backlog to the cap plus the deliveries in
	// flight — a soft bound, which is what a vendor's retry needs.
	if limit := s.cfg.ConnectorIntakeBacklogMax; limit > 0 {
		n, err := s.connectorIntakes.CountBacklog(ctx, cfg.TenantID)
		if err != nil {
			s.warnIngress(cfg, srcIP, "backlog count failed", err)
			httpError(w, http.StatusServiceUnavailable, "the intake store is unavailable")
			return
		}
		if n >= limit {
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.WebhookThrottledTotal.WithLabelValues(string(cfg.Provider), metrics.WebhookThrottledBacklogFull).Inc()
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(connectorBacklogRetryAfter.Seconds())))
			httpError(w, http.StatusServiceUnavailable, "the team's connector backlog is full (%d unprocessed deliveries, cap %d, %s)", n, limit, intake.EnvBacklogMax)
			return
		}
	}
	release, ok := s.chargeWebhookQuota(w, r, cfg)
	if !ok {
		return
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.ingressWriteBound())
	defer cancel()
	_, created, err := s.connectorIntakes.Accept(storeCtx, intake.Intake{
		TenantID:     cfg.TenantID,
		ConnectionID: b.conn.ID,
		IngressID:    cfg.ID,
		TriggerID:    d.TriggerID,
		DedupKey:     d.DedupKey,
		Event:        d.Event,
		ReceivedAt:   now,
		ExpiresAt:    intake.ExpiryFor(now, s.cfg.ConnectorIntakeRetention),
	})
	if err != nil {
		s.warnIngress(cfg, srcIP, "intake write failed", err)
		// The write's answer was lost, not necessarily the write: a row
		// that landed is the delivery stored — charged when this call wrote
		// it, a concurrent twin's otherwise. The unit goes back for a twin's
		// row and for a row proven absent. The re-read runs under its own
		// bound: the write's may be the very deadline that failed it.
		readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), s.ingressWriteBound())
		defer cancelRead()
		switch _, gerr := s.connectorIntakes.GetIntake(readCtx, cfg.TenantID, intakeID); {
		case gerr == nil:
			if !created {
				release()
			}
			s.markWebhookUsed(cfg)
			writeIngressAck(w, d.Ack)
			return
		case errors.Is(gerr, intake.ErrNotFound):
			release()
		}
		httpError(w, http.StatusServiceUnavailable, "the delivery could not be stored; retry")
		return
	}
	if !created {
		// A concurrent twin of this delivery wrote the row first: it was
		// charged, this one is the redelivery.
		release()
	}
	s.markWebhookUsed(cfg)
	writeIngressAck(w, d.Ack)
}

// ingressWriteBound is the bound of the intake write and of its re-read
// (ingressWriteTimeout unless a test sets ingressWriteLimit).
func (s *Server) ingressWriteBound() time.Duration {
	if s.ingressWriteLimit > 0 {
		return s.ingressWriteLimit
	}
	return ingressWriteTimeout
}

// ingressNow is the lane's clock (ingressClock).
func (s *Server) ingressNow() time.Time {
	if s.ingressClock != nil {
		return s.ingressClock().UTC()
	}
	return time.Now().UTC()
}

// writeIngressAck answers with the package's declared acknowledgement, its
// status already defaulted by the reader.
func writeIngressAck(w http.ResponseWriter, ack spec.IngressAck) {
	if ack.Body != "" {
		if json.Valid([]byte(ack.Body)) {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	w.WriteHeader(ack.Status)
	if ack.Body != "" {
		_, _ = io.WriteString(w, ack.Body)
	}
}

// warnIngress logs a refused delivery with its cause, bounded: the body never
// reaches the log, and a cause quoting the delivery is cut.
func (s *Server) warnIngress(cfg webhooks.Config, srcIP, what string, err error) {
	if s.logger != nil {
		s.logger.Warn("webhooks: connector ingress %s (team %s) from %s: %s: %s", cfg.ID, cfg.TenantID, srcIP, what, boundedText(err.Error(), maxIngressErrorText))
	}
}

// boundedText cuts s to at most n bytes, on a rune boundary, saying so.
func boundedText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… (%d bytes cut)", len(s)-cut)
}

// utf8RuneStart reports whether b starts a UTF-8 encoding.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
