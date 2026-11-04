package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/legal"
)

// legalDocumentView is the wire body of GET /api/legal/{slug}: the document
// as markdown, with the metadata a page renders beside it (the contact
// address and the accessibility declaration) so the page needs no second
// request.
type legalDocumentView struct {
	Slug     legal.Slug `json:"slug"`
	Title    string     `json:"title"`
	Markdown string     `json:"markdown"`
	// ContactEmail is the address every legal page offers (mailto:).
	ContactEmail  string              `json:"contact_email,omitempty"`
	Accessibility legal.Accessibility `json:"accessibility"`
}

// legalInfoBlock is the `legal` block of /api/server/info: what the SPA needs
// to decide which links to show — a document that is not configured has no
// link — plus the contact and the accessibility status the footer prints.
type legalInfoBlock struct {
	ContactEmail  string              `json:"contact_email,omitempty"`
	Accessibility legal.Accessibility `json:"accessibility"`
	Documents     []legalDocumentRef  `json:"documents"`
}

// legalDocumentRef names one configured document: its slug (the address
// segment under /legal/) and its title (the link text).
type legalDocumentRef struct {
	Slug  legal.Slug `json:"slug"`
	Title string     `json:"title"`
}

func (s *Server) legalInfo() legalInfoBlock {
	refs := make([]legalDocumentRef, 0, len(s.cfg.Legal.Documents))
	for _, d := range s.cfg.Legal.Documents {
		refs = append(refs, legalDocumentRef{Slug: d.Slug, Title: d.Title})
	}
	return legalInfoBlock{
		ContactEmail:  s.cfg.Legal.ContactEmail,
		Accessibility: s.cfg.Legal.Accessibility,
		Documents:     refs,
	}
}

// legalReadLimit is the token bucket of the public legal reads: generous
// enough for a page that loads once per visit, tight enough that an
// anonymous caller cannot turn a markdown document into a bandwidth tap.
var legalReadLimit = authBucketCfg{rate: 1, burst: 60} // 60/min sustained, burst 60

// registerLegalRoutes mounts the public legal surface (ADR-160). The route
// is public by path (isPublicPath) and rate-limited per client IP in a
// bucket of its own ("legal:<ip>"): its generous rate must not refill the
// login family's bare-IP budget. It is registered in every mode, and
// answers 404 for every slug when the deployment configured no document.
func (s *Server) registerLegalRoutes() {
	legalLimit := s.limitRouteScoped("legal", legalReadLimit, nil)
	s.mux.HandleFunc("GET /api/legal/{slug}", legalLimit(s.handleLegalDocument))
}

// handleLegalDocument answers GET /api/legal/{slug}. A slug that names no
// document and a known slug the deployment did not configure both 404: the
// first is not an address, the second is not published, and neither is a
// page the SPA should link to. The ETag is the document's own, so a browser
// that holds the page revalidates it with a 304.
func (s *Server) handleLegalDocument(w http.ResponseWriter, r *http.Request) {
	slug := legal.Slug(r.PathValue("slug"))
	if _, known := legal.Title(slug); !known {
		s.httpErrorFor(w, r, http.StatusNotFound, "no such legal document: %q", slug)
		return
	}
	doc, ok := s.cfg.Legal.Document(slug)
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "legal document %q is not published on this deployment", slug)
		return
	}
	w.Header().Set("ETag", doc.ETag)
	w.Header().Set("Cache-Control", "no-cache")
	if etagSatisfied(r.Header.Get("If-None-Match"), doc.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.writeJSONFor(w, r, legalDocumentView{
		Slug:          doc.Slug,
		Title:         doc.Title,
		Markdown:      doc.Markdown,
		ContactEmail:  s.cfg.Legal.ContactEmail,
		Accessibility: s.cfg.Legal.Accessibility,
	})
}
