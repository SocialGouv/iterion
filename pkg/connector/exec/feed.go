package exec

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// feedAccept is the Accept header of a feed operation that names none.
const feedAccept = "application/rss+xml, application/atom+xml;q=0.9, application/xml;q=0.8, text/xml;q=0.7"

// The bounds of one decoded feed. The executor bounds an answer's BYTES
// (32 MiB); a feed of empty <item/> elements fits millions of items in that,
// each a decoded map, so the item count and the nesting are bounded too —
// both refused past their bound, never truncated: a cut list would read as
// complete. Each is the operator's dial: 0 removes it, and a value that is
// not a count refuses the call rather than falling back.
const (
	EnvFeedMaxItems     = "ITERION_CONNECTOR_FEED_MAX_ITEMS"
	FeedMaxItemsDefault = 10000
	EnvFeedMaxDepth     = "ITERION_CONNECTOR_FEED_MAX_DEPTH"
	FeedMaxDepthDefault = 256
)

func feedBound(env string, def int) (int, error) {
	v := os.Getenv(env)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is not a count (0 removes the bound)", env, v)
	}
	return n, nil
}

// decodeFeed reads an RSS 2.0 or Atom 1.0 document into the one shape a poll
// reads, whichever the vendor serves:
//
//	{"title", "link", "updated",
//	 "items": [{"id", "version", "title", "link", "summary", "content",
//	            "published", "updated", "author", "categories"}]}
//
// An item's id falls back from its guid (or Atom id) to its link to the
// digest of what a reader sees; its version is that digest, so an edited
// entry is a new revision and an unchanged one is not (ADR-168 §7). Instants
// spec.ParseInstant reads are rewritten as RFC 3339 in UTC; one it does not
// is kept as written.
//
// The charset is the Content-Type header's when it names one (RFC 7303),
// else the document's own declaration; the labels read are WHATWG's for
// UTF-8 and windows-1252 (which Latin-1 and US-ASCII labels map to), and any
// other is refused by name. encoding/xml expands no DTD and resolves no
// external entity; HTML's named entities are accepted because feeds use them
// unescaped.
func decodeFeed(body []byte, contentType string) (any, error) {
	maxItems, err := feedBound(EnvFeedMaxItems, FeedMaxItemsDefault)
	if err != nil {
		return nil, err
	}
	maxDepth, err := feedBound(EnvFeedMaxDepth, FeedMaxDepthDefault)
	if err != nil {
		return nil, err
	}
	body, fromHeader, err := feedHeaderCharset(body, contentType)
	if err != nil {
		return nil, err
	}
	root, err := scanFeed(body, fromHeader, maxItems, maxDepth)
	if err != nil {
		return nil, err
	}
	switch {
	case root.Local == "rss" && root.Space == "":
		var doc rssDoc
		if err := newFeedDecoder(body, fromHeader).Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode the RSS document: %w", err)
		}
		if v := strings.TrimSpace(doc.Version); v != "" && !strings.HasPrefix(v, "2.") {
			return nil, fmt.Errorf("RSS version %q is not 2.x", v)
		}
		if doc.Channel == nil {
			// Prevents: a document that is not a feed read as an empty one,
			// which a poller takes for "nothing new".
			return nil, errors.New("the RSS document has no <channel>")
		}
		return doc.Channel.normalize(), nil
	case root.Local == "feed" && root.Space == atomNS:
		var doc atomFeed
		if err := newFeedDecoder(body, fromHeader).Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode the Atom document: %w", err)
		}
		return doc.normalize(), nil
	}
	return nil, fmt.Errorf("the root element <%s> (namespace %q) is neither RSS 2.0 <rss> nor Atom 1.0 <feed>", root.Local, root.Space)
}

const (
	atomNS    = "http://www.w3.org/2005/Atom"
	dcNS      = "http://purl.org/dc/elements/1.1/"
	contentNS = "http://purl.org/rss/1.0/modules/content/"
)

// scanFeed reads the document once, as tokens, before anything is decoded
// into a tree: the root's name, the nesting depth and the number of items,
// each refused past its bound, and nothing after the root element.
func scanFeed(body []byte, fromHeader bool, maxItems, maxDepth int) (xml.Name, error) {
	d := newFeedDecoder(body, fromHeader)
	var root xml.Name
	depth, items, closed := 0, 0, false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return xml.Name{}, fmt.Errorf("read the document: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if closed {
				return xml.Name{}, fmt.Errorf("the document holds content after its root element <%s>", root.Local)
			}
			if depth == 0 {
				root = t.Name
			}
			depth++
			if maxDepth > 0 && depth > maxDepth {
				return xml.Name{}, fmt.Errorf("the document nests deeper than %d elements (%s raises the bound)", maxDepth, EnvFeedMaxDepth)
			}
			if t.Name.Local == "item" || t.Name.Local == "entry" {
				items++
				if maxItems > 0 && items > maxItems {
					return xml.Name{}, fmt.Errorf("the feed holds more than %d items (%s raises the bound)", maxItems, EnvFeedMaxItems)
				}
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				closed = true
			}
		case xml.CharData:
			if closed && len(bytes.TrimSpace(t)) > 0 {
				return xml.Name{}, fmt.Errorf("the document holds text after its root element <%s>", root.Local)
			}
		}
	}
	if root.Local == "" {
		return xml.Name{}, errors.New("the document holds no element")
	}
	return root, nil
}

func newFeedDecoder(body []byte, fromHeader bool) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Entity = xml.HTMLEntity
	if fromHeader {
		// The header's charset already transcoded the body to UTF-8, and it
		// wins over the document's own declaration (RFC 7303).
		d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	} else {
		d.CharsetReader = feedDeclaredCharset
	}
	return d
}

// The WHATWG labels of the two encodings read. Latin-1 and US-ASCII labels
// are windows-1252's: a feed declaring ISO-8859-1 over windows-1252 bytes is
// how feeds are served, and the two agree outside 0x80–0x9F.
var (
	utf8Labels   = []string{"unicode-1-1-utf-8", "unicode11utf8", "unicode20utf8", "utf-8", "utf8", "x-unicode20utf8"}
	cp1252Labels = []string{"ansi_x3.4-1968", "ascii", "cp1252", "cp819", "csisolatin1", "ibm819", "iso-8859-1", "iso-ir-100",
		"iso8859-1", "iso88591", "iso_8859-1", "iso_8859-1:1987", "l1", "latin1", "us-ascii", "windows-1252", "x-cp1252"}
)

// feedLabel classifies a charset label: utf-8, windows-1252, or unknown.
func feedLabel(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case slices.Contains(utf8Labels, l):
		return "utf-8"
	case slices.Contains(cp1252Labels, l):
		return "windows-1252"
	}
	return ""
}

// feedHeaderCharset honours a charset the Content-Type names: the body comes
// back as UTF-8, and true says the document's own declaration is overridden.
func feedHeaderCharset(body []byte, contentType string) ([]byte, bool, error) {
	if contentType == "" {
		return body, false, nil
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["charset"] == "" {
		return body, false, nil
	}
	switch feedLabel(params["charset"]) {
	case "utf-8":
		return body, true, nil
	case "windows-1252":
		return cp1252ToUTF8(body), true, nil
	}
	return nil, false, fmt.Errorf("the answer's Content-Type names charset %q; UTF-8 and windows-1252 (Latin-1, US-ASCII) are read", params["charset"])
}

// feedDeclaredCharset transcodes what a document's own declaration names.
// encoding/xml calls it only for a declared charset other than "utf-8".
func feedDeclaredCharset(label string, in io.Reader) (io.Reader, error) {
	switch feedLabel(label) {
	case "utf-8":
		return in, nil
	case "windows-1252":
		raw, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(cp1252ToUTF8(raw)), nil
	}
	return nil, fmt.Errorf("the feed declares charset %q; UTF-8 and windows-1252 (Latin-1, US-ASCII) are read", label)
}

func cp1252ToUTF8(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+len(raw)/4)
	for _, b := range raw {
		r := rune(b)
		if b >= 0x80 && b <= 0x9f {
			r = cp1252[b-0x80]
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

// cp1252 maps windows-1252's 0x80–0x9F block; the rest of it is Latin-1.
var cp1252 = [0x20]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ',
}

// eachChild calls fn for each child element of the element start opened,
// and returns at its end. fn must consume the child (decode or Skip).
func eachChild(d *xml.Decoder, fn func(xml.StartElement) error) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := fn(t); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// text reads one element's character data.
func text(d *xml.Decoder, se xml.StartElement) (string, error) {
	var s string
	err := d.DecodeElement(&s, &se)
	return s, err
}

// firstText keeps the first non-empty value of an element RSS declares once.
func firstText(d *xml.Decoder, se xml.StartElement, cur string) (string, error) {
	s, err := text(d, se)
	if err != nil || strings.TrimSpace(cur) != "" {
		return cur, err
	}
	return s, nil
}

// RSS 2.0 elements carry no namespace. They are matched on (namespace, name)
// rather than by struct tags, which match a name in ANY namespace: an
// <atom:link rel="self"> after <link> would replace the channel's link, an
// <itunes:author> the author, a <media:description> the summary.

type rssDoc struct {
	Version string
	Channel *rssChannel
}

func (r *rssDoc) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, a := range start.Attr {
		if a.Name.Space == "" && a.Name.Local == "version" {
			r.Version = a.Value
		}
	}
	return eachChild(d, func(se xml.StartElement) error {
		if se.Name.Space == "" && se.Name.Local == "channel" && r.Channel == nil {
			var c rssChannel
			if err := d.DecodeElement(&c, &se); err != nil {
				return err
			}
			r.Channel = &c
			return nil
		}
		return d.Skip()
	})
}

type rssChannel struct {
	Title, Link, LastBuildDate string
	Items                      []rssItem
}

func (c *rssChannel) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	return eachChild(d, func(se xml.StartElement) error {
		if se.Name.Space != "" {
			return d.Skip()
		}
		var err error
		switch se.Name.Local {
		case "title":
			c.Title, err = firstText(d, se, c.Title)
		case "link":
			c.Link, err = firstText(d, se, c.Link)
		case "lastBuildDate":
			c.LastBuildDate, err = firstText(d, se, c.LastBuildDate)
		case "item":
			var it rssItem
			if err = d.DecodeElement(&it, &se); err == nil {
				c.Items = append(c.Items, it)
			}
		default:
			err = d.Skip()
		}
		return err
	})
}

type rssItem struct {
	Title, Link, Description, GUID, PubDate, Author, Creator, Content string
	Categories                                                        []string
}

func (it *rssItem) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	return eachChild(d, func(se xml.StartElement) error {
		var err error
		switch {
		case se.Name.Space == "":
			switch se.Name.Local {
			case "title":
				it.Title, err = firstText(d, se, it.Title)
			case "link":
				it.Link, err = firstText(d, se, it.Link)
			case "description":
				it.Description, err = firstText(d, se, it.Description)
			case "guid":
				it.GUID, err = firstText(d, se, it.GUID)
			case "pubDate":
				it.PubDate, err = firstText(d, se, it.PubDate)
			case "author":
				it.Author, err = firstText(d, se, it.Author)
			case "category":
				var s string
				if s, err = text(d, se); err == nil {
					it.Categories = append(it.Categories, s)
				}
			default:
				err = d.Skip()
			}
		case se.Name.Space == dcNS && se.Name.Local == "creator":
			it.Creator, err = firstText(d, se, it.Creator)
		case se.Name.Space == contentNS && se.Name.Local == "encoded":
			it.Content, err = firstText(d, se, it.Content)
		default:
			err = d.Skip()
		}
		return err
	})
}

func (c *rssChannel) normalize() map[string]any {
	items := make([]any, 0, len(c.Items))
	for _, it := range c.Items {
		author := it.Author
		if strings.TrimSpace(author) == "" {
			author = it.Creator
		}
		items = append(items, feedItem(it.GUID, it.Link, it.Title, it.Description, it.Content,
			feedInstant(it.PubDate), "", author, "", it.Categories))
	}
	return map[string]any{
		"title":   strings.TrimSpace(c.Title),
		"link":    strings.TrimSpace(c.Link),
		"updated": feedInstant(c.LastBuildDate),
		"items":   items,
	}
}

type atomFeed struct {
	Title   atomText     `xml:"http://www.w3.org/2005/Atom title"`
	Updated string       `xml:"http://www.w3.org/2005/Atom updated"`
	Links   []atomLink   `xml:"http://www.w3.org/2005/Atom link"`
	Authors []atomPerson `xml:"http://www.w3.org/2005/Atom author"`
	Entries []atomEntry  `xml:"http://www.w3.org/2005/Atom entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomPerson struct {
	Name string `xml:"http://www.w3.org/2005/Atom name"`
}

type atomEntry struct {
	ID         string       `xml:"http://www.w3.org/2005/Atom id"`
	Title      atomText     `xml:"http://www.w3.org/2005/Atom title"`
	Updated    string       `xml:"http://www.w3.org/2005/Atom updated"`
	Published  string       `xml:"http://www.w3.org/2005/Atom published"`
	Summary    atomText     `xml:"http://www.w3.org/2005/Atom summary"`
	Content    atomText     `xml:"http://www.w3.org/2005/Atom content"`
	Links      []atomLink   `xml:"http://www.w3.org/2005/Atom link"`
	Authors    []atomPerson `xml:"http://www.w3.org/2005/Atom author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"http://www.w3.org/2005/Atom category"`
}

// atomText is an Atom text construct (RFC 4287 §3.1): text and html are
// character data; xhtml — and any XML media type of a content — is markup,
// which character data alone would read as nothing.
type atomText struct {
	Type  string `xml:"type,attr"`
	Text  string `xml:",chardata"`
	Inner string `xml:",innerxml"`
}

func (t atomText) value() string {
	typ := strings.ToLower(strings.TrimSpace(t.Type))
	if typ == "xhtml" || strings.HasSuffix(typ, "/xml") || strings.HasSuffix(typ, "+xml") {
		return unwrapXHTML(t.Inner)
	}
	return t.Text
}

// unwrapXHTML drops the <div> an xhtml construct is wrapped in, which RFC
// 4287 §4.1.3.3 says is not part of the content.
func unwrapXHTML(inner string) string {
	s := strings.TrimSpace(inner)
	if !strings.HasPrefix(s, "<div") || !strings.HasSuffix(s, "</div>") {
		return s
	}
	open := strings.IndexByte(s, '>')
	if open < 0 {
		return s
	}
	return strings.TrimSpace(s[open+1 : len(s)-len("</div>")])
}

// alternate is the entry's page: the rel="alternate" link, or the one with
// no rel, which Atom defines as alternate.
func alternate(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	return ""
}

func firstName(people []atomPerson) string {
	for _, p := range people {
		if n := strings.TrimSpace(p.Name); n != "" {
			return n
		}
	}
	return ""
}

func (f atomFeed) normalize() map[string]any {
	// An entry with no author has the feed's (RFC 4287 §4.2.1).
	feedAuthor := firstName(f.Authors)
	items := make([]any, 0, len(f.Entries))
	for _, e := range f.Entries {
		cats := make([]string, 0, len(e.Categories))
		for _, c := range e.Categories {
			cats = append(cats, c.Term)
		}
		items = append(items, feedItem(e.ID, alternate(e.Links), e.Title.value(), e.Summary.value(), e.Content.value(),
			feedInstant(e.Published), feedInstant(e.Updated), firstName(e.Authors), feedAuthor, cats))
	}
	return map[string]any{
		"title":   strings.TrimSpace(f.Title.value()),
		"link":    alternate(f.Links),
		"updated": feedInstant(f.Updated),
		"items":   items,
	}
}

// feedItem builds one normalized item. Its version is the digest of what the
// entry itself says — every field, each length-prefixed so two fields cannot
// trade bytes, categories in sorted order since their order means nothing;
// its id is the vendor's, else the link, else that digest. An entry with no
// author shows the one it inherits (an Atom feed's), which its version does
// not hash: a feed-level edit would otherwise be a new revision of every
// entry, and the whole feed delivered again.
func feedItem(id, link, title, summary, content, published, updated, author, inherited string, categories []string) map[string]any {
	id, link = strings.TrimSpace(id), strings.TrimSpace(link)
	title, author = strings.TrimSpace(title), strings.TrimSpace(author)
	summary, content = strings.TrimSpace(summary), strings.TrimSpace(content)
	cats := make([]string, 0, len(categories))
	for _, c := range categories {
		if c = strings.TrimSpace(c); c != "" {
			cats = append(cats, c)
		}
	}
	h := sha256.New()
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	// The TEXT a reader sees, not the markup that renders it: a publisher
	// that stamps per-render tokens into its HTML (WordPress writes uniqid()
	// into its gallery and lightbox blocks) would otherwise give an unedited
	// entry a new revision on every read.
	for _, f := range []string{visibleText(title), link, visibleText(summary), visibleText(content), published, updated, author} {
		field(f)
	}
	sorted := slices.Clone(cats)
	slices.Sort(sorted)
	for _, c := range sorted {
		field(c)
	}
	version := "sha256:" + hex.EncodeToString(h.Sum(nil))
	switch {
	case id != "":
	case link != "":
		id = link
	default:
		id = version
	}
	listed := make([]any, len(cats))
	for i, c := range cats {
		listed[i] = c
	}
	if author == "" {
		author = strings.TrimSpace(inherited)
	}
	item := map[string]any{"id": id, "version": version, "categories": listed}
	// A field the entry does not carry is absent, not "": an output reading
	// it is then unset (ADR-168 §9), never a var set to nothing.
	for k, v := range map[string]string{
		"title": title, "link": link, "summary": summary, "content": content,
		"published": published, "updated": updated, "author": author,
	} {
		if v != "" {
			item[k] = v
		}
	}
	return item
}

// feedInstant rewrites an instant as RFC 3339 in UTC, through the one
// reading every cursor shares (spec.ParseInstant), or returns it as written
// when that reading refuses it — never an empty string for a non-empty value,
// which a timestamp cursor would compare as the zero time.
func feedInstant(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := spec.ParseInstant(s); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return s
}

// markup matches a tag, its attributes included.
var markup = regexp.MustCompile(`<[^>]*>`)

// visibleText is what an HTML fragment says once rendered: its tags and
// attributes dropped, its entities decoded, its runs of white space one
// space. It versions an entry; the entry itself keeps its markup.
func visibleText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(markup.ReplaceAllString(s, " "))), " ")
}
