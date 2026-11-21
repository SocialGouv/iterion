package store

import (
	"crypto/rand"
	"math"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// UsageSource names the harness seam; generic reporting attests no model route.
type UsageSource string

const (
	UsageDelegate             UsageSource = "delegate"
	UsageStructuredExtraction UsageSource = "structured_extraction"
	UsageGenericReported      UsageSource = "generic_reported"
)

type UsageCostKind string

const (
	UsageAdditiveCost UsageCostKind = "additive"
	UsageSessionTotal UsageCostKind = "session_total"
)

// InvocationUsageRoute describes a backend invocation, NOT a distribution of
// its usage among provider-internal models. ReportedModel is harness metadata,
// not exhaustive served-model attribution. Fingerprint is never a credential.
type InvocationUsageRoute struct {
	Scope              string `json:"scope" bson:"scope"`
	Backend            string `json:"backend,omitempty" bson:"backend,omitempty"`
	RequestedModel     string `json:"requested_model,omitempty" bson:"requested_model,omitempty"`
	ReportedModel      string `json:"reported_model,omitempty" bson:"reported_model,omitempty"`
	Provider           string `json:"provider,omitempty" bson:"provider,omitempty"`
	SessionID          string `json:"session_id,omitempty" bson:"session_id,omitempty"`
	SessionFingerprint string `json:"session_fingerprint,omitempty" bson:"session_fingerprint,omitempty"`
}

// UsageTokenSplit preserves reported input/output and cache counters. Cache
// counters are separate observations, not additional aggregate Tokens.
type UsageTokenSplit struct {
	Input      int `json:"input" bson:"input"`
	Output     int `json:"output" bson:"output"`
	CacheRead  int `json:"cache_read" bson:"cache_read"`
	CacheWrite int `json:"cache_write" bson:"cache_write"`
}

// ReportedEconomicSession identifies a cumulative counter declared by a generic
// executor. It attests no model, provider or credential route.
// Fork inheritance requires a separate immutable economic-origin contract.
type ReportedEconomicSession struct {
	Backend     string `json:"backend" bson:"backend"`
	SessionID   string `json:"session_id" bson:"session_id"`
	Fingerprint string `json:"fingerprint,omitempty" bson:"fingerprint,omitempty"`
}
type UsageComponent struct {
	ReportedSession *ReportedEconomicSession `json:"reported_session,omitempty" bson:"reported_session,omitempty"`
	Ordinal         uint64                   `json:"ordinal" bson:"ordinal"`
	Source          UsageSource              `json:"source" bson:"source"`
	Route           *InvocationUsageRoute    `json:"route,omitempty" bson:"route,omitempty"`
	CallID          string                   `json:"call_id,omitempty" bson:"call_id,omitempty"`
	Outcome         string                   `json:"outcome,omitempty" bson:"outcome,omitempty"`
	Tokens          int                      `json:"tokens" bson:"tokens"`
	TokenSplit      *UsageTokenSplit         `json:"token_split,omitempty" bson:"token_split,omitempty"`
	UnpricedTokens  int                      `json:"unpriced_tokens" bson:"unpriced_tokens"`
	UnreportedCalls int                      `json:"unreported_calls" bson:"unreported_calls"`
	CostKnown       bool                     `json:"cost_known" bson:"cost_known"`
	CostKind        UsageCostKind            `json:"cost_kind" bson:"cost_kind"`
	CostUSD         float64                  `json:"cost_usd" bson:"cost_usd"`
}

func CloneUsageComponents(in []UsageComponent) []UsageComponent {
	if len(in) == 0 {
		return nil
	}
	out := append([]UsageComponent(nil), in...)
	for i := range out {
		if out[i].ReportedSession != nil {
			v := *out[i].ReportedSession
			out[i].ReportedSession = &v
		}
		if out[i].Route != nil {
			v := *out[i].Route
			out[i].Route = &v
		}
		if out[i].TokenSplit != nil {
			v := *out[i].TokenSplit
			out[i].TokenSplit = &v
		}
	}
	return out
}
func finiteUsageAmount(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func ValidateUsageComponent(c UsageComponent) error {
	if c.Ordinal == 0 || c.Ordinal > math.MaxInt64 || c.Tokens < 0 || c.UnpricedTokens < 0 || c.UnpricedTokens > c.Tokens || c.UnreportedCalls < 0 || !finiteUsageAmount(c.CostUSD) {
		return invalidCorrection("invalid usage component counters")
	}
	if c.CostKnown && c.UnpricedTokens != 0 || !c.CostKnown && (c.CostUSD != 0 || c.UnpricedTokens != c.Tokens) {
		return invalidCorrection("inconsistent component pricing presence")
	}
	if !utf8.ValidString(c.CallID) || !utf8.ValidString(c.Outcome) {
		return invalidCorrection("invalid component provenance")
	}
	if c.Route != nil {
		r := c.Route
		if r.Scope != "invocation" {
			return invalidCorrection("unknown usage route scope")
		}
		for _, v := range []string{r.Backend, r.RequestedModel, r.ReportedModel, r.Provider, r.SessionID, r.SessionFingerprint} {
			if !utf8.ValidString(v) {
				return invalidCorrection("invalid usage route")
			}
		}
	}
	if r := c.ReportedSession; r != nil {
		if c.Source != UsageGenericReported || c.CostKind != UsageSessionTotal || r.Backend == "" || r.SessionID == "" || !utf8.ValidString(r.Backend) || !utf8.ValidString(r.SessionID) || !utf8.ValidString(r.Fingerprint) {
			return invalidCorrection("invalid declared economic session")
		}
	}
	switch c.Source {
	case UsageDelegate:
		if c.Route == nil || c.CallID != "" || c.Outcome != "" {
			return invalidCorrection("invalid delegate provenance")
		}
	case UsageStructuredExtraction:
		if c.Route == nil || c.Route.Backend != "claw" || c.Route.RequestedModel == "" || c.CallID == "" || c.TokenSplit == nil || (c.Outcome != "success" && c.Outcome != "error") || c.CostKind != UsageAdditiveCost || c.Route.SessionID != "" || c.Route.SessionFingerprint != "" {
			return invalidCorrection("invalid extraction provenance")
		}
	case UsageGenericReported:
		if c.Route != nil || c.CallID != "" || c.Outcome != "" || c.TokenSplit != nil {
			return invalidCorrection("generic reporting cannot attest route")
		}
	default:
		return invalidCorrection("unknown usage component source")
	}
	if s := c.TokenSplit; s != nil {
		if s.Input < 0 || s.Output < 0 || s.CacheRead < 0 || s.CacheWrite < 0 || s.Output > math.MaxInt-s.Input || s.Input+s.Output != c.Tokens {
			return invalidCorrection("invalid token split")
		}
	}
	switch c.CostKind {
	case UsageAdditiveCost:
	case UsageSessionTotal:
		if c.ReportedSession == nil && (c.Route == nil || c.Route.Backend == "" || c.Route.SessionID == "") {
			return invalidCorrection("session component lacks economic identity")
		}
	default:
		return invalidCorrection("unknown component cost kind")
	}
	return nil
}
func componentSession(c UsageComponent) SessionCostObservation {
	if r := c.ReportedSession; r != nil {
		return SessionCostObservation{Backend: r.Backend, SessionID: r.SessionID, Fingerprint: r.Fingerprint, CostUSD: c.CostUSD}
	}
	r := c.Route
	return SessionCostObservation{Backend: r.Backend, SessionID: r.SessionID, Fingerprint: r.SessionFingerprint, CostUSD: c.CostUSD}
}

// SummarizeUsageComponents reduces only this disjoint lot, in its fixed order.
// It never prices cumulative readings; that requires the store's atomic marks.
func SummarizeUsageComponents(collectionID string, components []UsageComponent) (UsageObservation, error) {
	if collectionID == "" || !utf8.ValidString(collectionID) {
		return UsageObservation{}, invalidCorrection("missing usage collection identity")
	}
	out := UsageObservation{Version: 2, CollectionID: collectionID, Components: CloneUsageComponents(components)}
	sessions := map[string]SessionCostObservation{}
	calls := map[string]bool{}
	var prior uint64
	for _, c := range out.Components {
		if err := ValidateUsageComponent(c); err != nil {
			return UsageObservation{}, err
		}
		if c.Ordinal <= prior {
			return UsageObservation{}, invalidCorrection("usage component ordinals repeated or unordered")
		}
		prior = c.Ordinal
		if c.CallID != "" {
			if calls[c.CallID] {
				return UsageObservation{}, invalidCorrection("duplicate extraction call ID")
			}
			calls[c.CallID] = true
		}
		if c.Tokens > math.MaxInt-out.Tokens || c.UnpricedTokens > math.MaxInt-out.UnpricedTokens || c.UnreportedCalls > math.MaxInt-out.UnreportedCalls {
			return UsageObservation{}, invalidCorrection("usage component overflow")
		}
		out.Tokens += c.Tokens
		out.UnpricedTokens += c.UnpricedTokens
		out.UnreportedCalls += c.UnreportedCalls
		if c.CostKind == UsageSessionTotal {
			s := componentSession(c)
			key := sessionCostKey(s)
			if old, ok := sessions[key]; !ok || s.CostUSD > old.CostUSD {
				sessions[key] = s
			}
		} else {
			out.OtherCostUSD += c.CostUSD
			if !finiteUsageAmount(out.OtherCostUSD) {
				return UsageObservation{}, invalidCorrection("usage cost overflow")
			}
		}
	}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, s)
	}
	slices.SortFunc(out.Sessions, func(a, b SessionCostObservation) int { return strings.Compare(sessionCostKey(a), sessionCostKey(b)) })
	return out, nil
}

// UsageProjectionLine has exactly one provenance: an invocation component, or
// a session aggregate. Only CostUSD on the LINE is marginal spend. A session
// component's raw cumulative reading remains in Observation, not as a charge.
type UsageProjectionLine struct {
	ReportedSession *ReportedEconomicSession `json:"reported_session,omitempty" bson:"reported_session,omitempty"`
	Kind            string                   `json:"kind" bson:"kind"`
	Ordinal         uint64                   `json:"ordinal,omitempty" bson:"ordinal,omitempty"`
	Source          UsageSource              `json:"source,omitempty" bson:"source,omitempty"`
	Route           *InvocationUsageRoute    `json:"route,omitempty" bson:"route,omitempty"`
	CallID          string                   `json:"call_id,omitempty" bson:"call_id,omitempty"`
	Tokens          int                      `json:"tokens,omitempty" bson:"tokens,omitempty"`
	TokenSplit      *UsageTokenSplit         `json:"token_split,omitempty" bson:"token_split,omitempty"`
	UnpricedTokens  int                      `json:"unpriced_tokens,omitempty" bson:"unpriced_tokens,omitempty"`
	UnreportedCalls int                      `json:"unreported_calls,omitempty" bson:"unreported_calls,omitempty"`
	CostKnown       bool                     `json:"cost_known" bson:"cost_known"`
	CostUSD         float64                  `json:"cost_usd" bson:"cost_usd"`
	Session         *SessionCostObservation  `json:"session,omitempty" bson:"session,omitempty"`
	PreviousMaxUSD  float64                  `json:"previous_max_usd,omitempty" bson:"previous_max_usd,omitempty"`
}
type UsageProjection struct {
	CollectionID string                `json:"collection_id" bson:"collection_id"`
	Version      int                   `json:"version" bson:"version"`
	Lines        []UsageProjectionLine `json:"lines" bson:"lines"`
}

func cloneUsageProjection(in *UsageProjection) *UsageProjection {
	if in == nil {
		return nil
	}
	out := *in
	out.Lines = append([]UsageProjectionLine(nil), in.Lines...)
	for i := range out.Lines {
		v := &out.Lines[i]
		if v.ReportedSession != nil {
			r := *v.ReportedSession
			v.ReportedSession = &r
		}
		if v.Route != nil {
			r := *v.Route
			v.Route = &r
		}
		if v.TokenSplit != nil {
			s := *v.TokenSplit
			v.TokenSplit = &s
		}
		if v.Session != nil {
			s := *v.Session
			v.Session = &s
		}
	}
	return &out
}
func projectUsage(obs UsageObservation, prior map[string]float64) (*UsageProjection, OutputCorrectionUsage, error) {
	p := &UsageProjection{Version: 2, CollectionID: obs.CollectionID}
	u := OutputCorrectionUsage{}
	for _, c := range obs.Components {
		line := UsageProjectionLine{Kind: "component", Ordinal: c.Ordinal, Source: c.Source, Route: c.Route, ReportedSession: c.ReportedSession, CallID: c.CallID, Tokens: c.Tokens, TokenSplit: c.TokenSplit, UnpricedTokens: c.UnpricedTokens, UnreportedCalls: c.UnreportedCalls, CostKnown: c.CostKnown}
		if c.CostKind == UsageAdditiveCost {
			line.CostUSD = c.CostUSD
		}
		p.Lines = append(p.Lines, line)
		u.Tokens += c.Tokens
		u.UnreportedCalls += c.UnreportedCalls
		u.CostUSD += line.CostUSD
	}
	for _, s := range obs.Sessions {
		old := prior[sessionCostKey(s)]
		if !finiteUsageAmount(old) {
			return nil, u, invalidCorrection("invalid durable session maximum")
		}
		known := true
		for _, c := range obs.Components {
			if c.CostKind == UsageSessionTotal && sessionCostKey(componentSession(c)) == sessionCostKey(s) && !c.CostKnown {
				known = false
			}
		}
		v := s
		line := UsageProjectionLine{Kind: "sessionAggregate", Session: &v, PreviousMaxUSD: old, CostKnown: known, CostUSD: max(0, s.CostUSD-old)}
		p.Lines = append(p.Lines, line)
		u.CostUSD += line.CostUSD
	}
	if !validCorrectionUsage(u) {
		return nil, u, invalidCorrection("projected usage overflow")
	}
	return cloneUsageProjection(p), u, nil
}

// FreezeSettledUsageReceipt validates persisted v1/v2 without consulting current
// session marks. V1 retains its exact stored Usage and absent projection.
func FreezeSettledUsageReceipt(raw OutputCorrectionReceipt) (OutputCorrectionReceipt, error) {
	if raw.Observation == nil || !validCorrectionUsage(raw.Usage) {
		return raw, invalidCorrection("invalid settled observation")
	}
	request := raw
	request.Usage = OutputCorrectionUsage{}
	request.Projection = nil
	frozen, err := normalizeObservationReceipt(request)
	if err != nil {
		return raw, err
	}
	if raw.Usage.Tokens != frozen.Observation.Tokens || raw.Usage.UnreportedCalls != frozen.Observation.UnreportedCalls {
		return raw, invalidCorrection("settled counters differ from observation")
	}
	if frozen.Observation.Version == 1 {
		if raw.Projection != nil {
			return raw, invalidCorrection("legacy receipt invented route projection")
		}
		frozen.Usage = raw.Usage
		return frozen, nil
	}
	if raw.Projection == nil || raw.Projection.Version != 2 {
		return raw, invalidCorrection("missing v2 usage projection")
	}
	prior := map[string]float64{}
	for _, line := range raw.Projection.Lines {
		if line.Kind == "sessionAggregate" && line.Session != nil {
			key := sessionCostKey(*line.Session)
			if _, ok := prior[key]; ok {
				return raw, invalidCorrection("duplicate session projection")
			}
			prior[key] = line.PreviousMaxUSD
		}
	}
	expected, u, err := projectUsage(*frozen.Observation, prior)
	if err != nil {
		return raw, err
	}
	if !reflect.DeepEqual(expected, raw.Projection) || u != raw.Usage {
		return raw, invalidCorrection("stored projection differs from settled facts")
	}
	frozen.Usage = u
	frozen.Projection = expected
	return frozen, nil
}

// CloneOutputCorrectionEpisode preserves store-owned observations/projections
// when a decision or retired-history copy crosses a caller boundary.
func CloneOutputCorrectionEpisode(in OutputCorrectionEpisode) OutputCorrectionEpisode {
	out := in
	out.Reservations = append([]OutputCorrectionReservation(nil), in.Reservations...)
	if in.Cause != nil {
		v := *in.Cause
		out.Cause = &v
	}
	if in.RetiredAt != nil {
		v := *in.RetiredAt
		out.RetiredAt = &v
	}
	for i := range out.Reservations {
		r := &out.Reservations[i]
		if r.Origin != nil {
			v := *r.Origin
			r.Origin = &v
		}
		if r.Receipt != nil {
			v := *r.Receipt
			if v.Observation != nil {
				o := *v.Observation
				o.Sessions = append([]SessionCostObservation(nil), o.Sessions...)
				o.Components = CloneUsageComponents(o.Components)
				v.Observation = &o
			}
			v.Projection = cloneUsageProjection(v.Projection)
			r.Receipt = &v
		}
	}
	return out
}

// NewUsageCollectionID names one collector lifetime. It is provenance only,
// never execution authority, a credential, or a financial posting receipt.
func NewUsageCollectionID() string { return rand.Text() }

func validateDisjointUsageComponents(r *Run, in OutputCorrectionReceipt, res OutputCorrectionReservation) error {
	if in.Observation == nil || in.Observation.Version != 2 {
		return nil
	}
	ordinals := map[uint64]bool{}
	calls := map[string]bool{}
	for _, c := range in.Observation.Components {
		ordinals[c.Ordinal] = true
		if c.CallID != "" {
			calls[c.CallID] = true
		}
	}
	check := func(other OutputCorrectionReservation) error {
		if other.Receipt == nil || other.Receipt.Observation == nil {
			return nil
		}
		obs := other.Receipt.Observation
		if obs.Version != 2 || obs.CollectionID != in.Observation.CollectionID {
			return nil
		}
		if other.InitialUsageID != res.InitialUsageID {
			return invalidCorrection("usage collection changed initial owner")
		}
		for _, c := range obs.Components {
			if ordinals[c.Ordinal] {
				return invalidCorrection("component already settled by another receipt")
			}
			if c.CallID != "" && calls[c.CallID] {
				return invalidCorrection("extraction call already settled by another receipt")
			}
		}
		return nil
	}
	for _, other := range r.InitialUsageReservations {
		if err := check(other); err != nil {
			return err
		}
	}
	for _, ep := range r.OutputCorrections {
		for _, other := range ep.Reservations {
			if err := check(other); err != nil {
				return err
			}
		}
	}
	for _, ep := range r.OutputCorrectionHistory {
		for _, other := range ep.Reservations {
			if err := check(other); err != nil {
				return err
			}
		}
	}
	return nil
}
