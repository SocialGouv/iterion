package exec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// feedAccept is the Accept header of a feed operation that names none.
const feedAccept = "application/rss+xml, application/atom+xml;q=0.9, application/xml;q=0.8, text/xml;q=0.7"

// decodeFeed reads an RSS 2.0 or Atom 1.0 document into the one shape a poll
// reads, whichever the vendor serves:
//
//	{"title", "link", "updated",
//	 "items": [{"id", "version", "title", "link", "summary", "content",
//	            "published", "updated", "author", "categories"}]}
//
// An item's id falls back from its guid (or Atom id) to its link to the
// digest of its content; its version is that content digest, so an edited
// entry is a new revision and an unchanged one is not (ADR-168 §7). Instants
// that parse are rewritten as RFC 3339, so a timestamp cursor compares one
// layout.
//
// encoding/xml expands no DTD and resolves no external entity; HTML's named
// entities are accepted because feeds use them unescaped. A charset other
// than UTF-8, US-ASCII, ISO-8859-1 or windows-1252 is refused by name.
func decodeFeed(body []byte) (any, error) {
	root, err := feedRoot(body)
	if err != nil {
		return nil, err
	}
	switch {
	case root.Local == "rss":
		var doc rssDoc
		if err := newFeedDecoder(body).Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode the RSS document: %w", err)
		}
		if v := strings.TrimSpace(doc.Version); v != "" && !strings.HasPrefix(v, "2.") {
			return nil, fmt.Errorf("RSS version %q is not 2.x", v)
		}
		return doc.normalize(), nil
	case root.Local == "feed" && root.Space == atomNS:
		var doc atomFeed
		if err := newFeedDecoder(body).Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode the Atom document: %w", err)
		}
		return doc.normalize(), nil
	}
	return nil, fmt.Errorf("the root element <%s> (namespace %q) is neither RSS 2.0 <rss> nor Atom 1.0 <feed>", root.Local, root.Space)
}

const atomNS = "http://www.w3.org/2005/Atom"

// feedRoot reads the first element's name and nothing else.
func feedRoot(body []byte) (xml.Name, error) {
	d := newFeedDecoder(body)
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return xml.Name{}, errors.New("the document holds no element")
			}
			return xml.Name{}, fmt.Errorf("read the document: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name, nil
		}
	}
}

func newFeedDecoder(body []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Entity = xml.HTMLEntity
	d.CharsetReader = feedCharset
	return d
}

// feedCharset transcodes the single-byte charsets feeds still declare.
// encoding/xml calls it only for a declared charset other than UTF-8.
func feedCharset(label string, in io.Reader) (io.Reader, error) {
	var table *[0x20]rune
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1":
	case "windows-1252", "cp1252":
		table = &cp1252
	default:
		return nil, fmt.Errorf("the feed declares charset %q; UTF-8, US-ASCII, ISO-8859-1 and windows-1252 are read", label)
	}
	raw, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(raw)+len(raw)/4)
	for _, b := range raw {
		r := rune(b)
		if table != nil && b >= 0x80 && b <= 0x9f {
			r = table[b-0x80]
		}
		out = utf8.AppendRune(out, r)
	}
	return bytes.NewReader(out), nil
}

// cp1252 maps windows-1252's 0x80–0x9F block; the rest of it is Latin-1.
var cp1252 = [0x20]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ',
}

type rssDoc struct {
	Version string `xml:"version,attr"`
	Channel struct {
		Title         string    `xml:"title"`
		Link          string    `xml:"link"`
		LastBuildDate string    `xml:"lastBuildDate"`
		Items         []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	Description string   `xml:"description"`
	GUID        string   `xml:"guid"`
	PubDate     string   `xml:"pubDate"`
	Author      string   `xml:"author"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Content     string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Categories  []string `xml:"category"`
}

func (d rssDoc) normalize() map[string]any {
	items := make([]any, 0, len(d.Channel.Items))
	for _, it := range d.Channel.Items {
		author := it.Author
		if author == "" {
			author = it.Creator
		}
		items = append(items, feedItem(it.GUID, it.Link, it.Title, it.Description, it.Content,
			feedInstant(it.PubDate), "", author, it.Categories))
	}
	return map[string]any{
		"title":   strings.TrimSpace(d.Channel.Title),
		"link":    strings.TrimSpace(d.Channel.Link),
		"updated": feedInstant(d.Channel.LastBuildDate),
		"items":   items,
	}
}

type atomFeed struct {
	Title   string      `xml:"http://www.w3.org/2005/Atom title"`
	Updated string      `xml:"http://www.w3.org/2005/Atom updated"`
	Links   []atomLink  `xml:"http://www.w3.org/2005/Atom link"`
	Entries []atomEntry `xml:"http://www.w3.org/2005/Atom entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomEntry struct {
	ID        string     `xml:"http://www.w3.org/2005/Atom id"`
	Title     string     `xml:"http://www.w3.org/2005/Atom title"`
	Updated   string     `xml:"http://www.w3.org/2005/Atom updated"`
	Published string     `xml:"http://www.w3.org/2005/Atom published"`
	Summary   string     `xml:"http://www.w3.org/2005/Atom summary"`
	Content   atomText   `xml:"http://www.w3.org/2005/Atom content"`
	Links     []atomLink `xml:"http://www.w3.org/2005/Atom link"`
	Authors   []struct {
		Name string `xml:"http://www.w3.org/2005/Atom name"`
	} `xml:"http://www.w3.org/2005/Atom author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"http://www.w3.org/2005/Atom category"`
}

// atomText keeps an xhtml content's markup, which chardata would drop.
type atomText struct {
	Type  string `xml:"type,attr"`
	Text  string `xml:",chardata"`
	Inner string `xml:",innerxml"`
}

func (t atomText) value() string {
	if t.Type == "xhtml" {
		return t.Inner
	}
	return t.Text
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

func (f atomFeed) normalize() map[string]any {
	items := make([]any, 0, len(f.Entries))
	for _, e := range f.Entries {
		var author string
		if len(e.Authors) > 0 {
			author = e.Authors[0].Name
		}
		cats := make([]string, 0, len(e.Categories))
		for _, c := range e.Categories {
			cats = append(cats, c.Term)
		}
		items = append(items, feedItem(e.ID, alternate(e.Links), e.Title, e.Summary, e.Content.value(),
			feedInstant(e.Published), feedInstant(e.Updated), author, cats))
	}
	return map[string]any{
		"title":   strings.TrimSpace(f.Title),
		"link":    alternate(f.Links),
		"updated": feedInstant(f.Updated),
		"items":   items,
	}
}

// feedItem builds one normalized item. Its version is the digest of what a
// reader sees; its id the vendor's, else the link, else that digest.
func feedItem(id, link, title, summary, content, published, updated, author string, categories []string) map[string]any {
	id, link = strings.TrimSpace(id), strings.TrimSpace(link)
	title, author = strings.TrimSpace(title), strings.TrimSpace(author)
	cats := make([]any, 0, len(categories))
	for _, c := range categories {
		if c = strings.TrimSpace(c); c != "" {
			cats = append(cats, c)
		}
	}
	canon, _ := json.Marshal([]string{title, link, summary, content, published, updated})
	sum := sha256.Sum256(canon)
	version := "sha256:" + hex.EncodeToString(sum[:])
	switch {
	case id != "":
	case link != "":
		id = link
	default:
		id = version
	}
	return map[string]any{
		"id": id, "version": version,
		"title": title, "link": link, "summary": summary, "content": content,
		"published": published, "updated": updated, "author": author,
		"categories": cats,
	}
}

// feedLayouts are the instants feeds write: RFC 3339 for Atom, the RFC 822
// family for RSS (with one- or two-digit days, numeric or named zones).
var feedLayouts = []string{
	time.RFC3339Nano,
	time.RFC1123Z, time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	time.RFC822Z, time.RFC822,
	"2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04:05 MST",
}

// feedInstant rewrites an instant as RFC 3339 in UTC, or returns it as
// written when no layout reads it — never an empty string for a non-empty
// value, which a timestamp cursor would compare as the zero time.
func feedInstant(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range feedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339Nano)
		}
	}
	return s
}
