package server

// GET /api/v1/connectors/{id}/icon serves a connector package's mark: the
// bytes the package load sanitized (pkg/connector/spec/icon.go), never the
// file's. The studio renders it as a bare <img>, which cannot add headers —
// the session rides the cookie — so the route sits behind requireAuth and
// resolves the catalog the way the requester's tenant composes it, exactly
// like the catalog read model does. An entry that ships no icon gets a
// monogram on the package's brand color, contrast-checked at generation.

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

func (s *Server) registerConnectorIconRoutes() {
	s.mux.Handle("GET /api/v1/connectors/{id}/icon", s.requireAuth(http.HandlerFunc(s.handleConnectorIcon)))
}

// iconCSP is the policy the icon response carries: the SVG may style
// itself inline and nothing else — no script, no fetch, no forms — and
// `sandbox` keeps a browser from ever treating the document as an active
// origin even if a sanitizer class were one day missed.
const iconCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

func (s *Server) handleConnectorIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.connectorCatalogFor(s.triggerTenant(r))
	if cat == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "the connector catalog is not wired on this deployment")
		return
	}
	pkg, _, err := cat.Package(r.PathValue("id"))
	if err != nil {
		switch {
		case connection.IsNoSuchConnector(err):
			s.httpErrorFor(w, r, http.StatusNotFound, "no connector %q in the catalog", r.PathValue("id"))
		case connection.IsCatalogUnavailable(err):
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "the connector catalog is unreachable: %v", err)
		default:
			s.httpErrorFor(w, r, http.StatusInternalServerError, "read the connector package: %v", err)
		}
		return
	}
	s.serveIconSVG(w, r, pkg)
}

// serveIconSVG writes the sanitized bytes, or the monogram when the
// package ships none, under the icon headers; ServeContent answers
// If-None-Match from the ETag set here.
func (s *Server) serveIconSVG(w http.ResponseWriter, r *http.Request, pkg *spec.Package) {
	body := pkg.IconSVG
	digest := pkg.IconDigest
	if len(body) == 0 {
		body = monogramSVG(pkg.Connector)
		sum := sha256.Sum256(body)
		digest = hex.EncodeToString(sum[:])[:16]
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Security-Policy", iconCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, "icon.svg", zeroTime, strings.NewReader(string(body)))
}

// monogramSVG renders the fallback mark: up to two initials, centered on a
// rounded tile of the package's brand color, in the foreground its
// luminance picks. Packages that declare neither get the neutral tile.
func monogramSVG(c spec.Connector) []byte {
	brand := c.BrandColor
	if !spec.ValidBrandColor(brand) {
		brand = "#2b303b"
	}
	label := monogramInitials(c.DisplayName, c.ID)
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" role="img" aria-label="` + label + `">` +
		`<rect width="64" height="64" rx="14" fill="` + brand + `"/>` +
		`<text x="32" y="41" font-family="system-ui, sans-serif" font-size="28" font-weight="600" text-anchor="middle" fill="` + monogramForeground(brand) + `">` + label + `</text>` +
		`</svg>`)
}

// monogramInitials takes the first letter of up to two words of the
// display name; a nameless package gets its id's first two letters.
func monogramInitials(displayName, id string) string {
	words := strings.Fields(strings.ToUpper(displayName))
	var b strings.Builder
	for _, w := range words {
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		b.WriteRune(r[0])
		if b.Len() >= 2 {
			break
		}
	}
	if b.Len() == 0 {
		for _, r := range strings.ToUpper(id) {
			b.WriteRune(r)
			if b.Len() >= 2 {
				break
			}
		}
	}
	return b.String()
}

// monogramForeground picks the tile's text color by contrast: the nearer
// of black and white to the brand color under the WCAG ratio, so a pale
// accent reads on its tile and a dark one too.
func monogramForeground(brand string) string {
	r, g, b := hexChannel(brand[1:3]), hexChannel(brand[3:5]), hexChannel(brand[5:7])
	l := relativeLuminance(r, g, b)
	contrastOnBlack := (l + 0.05) / 0.05
	contrastOnWhite := 1.05 / (l + 0.05)
	if contrastOnBlack >= contrastOnWhite {
		return "#000000"
	}
	return "#ffffff"
}

// hexChannel parses one hex byte pair; ValidBrandColor already judged the
// shape, so an error here is unreachable and reads as black.
func hexChannel(s string) float64 {
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0
	}
	return float64(v) / 255
}

// relativeLuminance linearizes the channels and weighs them the way the
// WCAG contrast ratio does.
func relativeLuminance(r, g, b float64) float64 {
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// zeroTime pins ServeContent's Last-Modified off: an icon's freshness is
// its digest, not a file mtime.
var zeroTime = time.Time{}
