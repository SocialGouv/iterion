# Polling an RSS or Atom feed

A feed has no webhook: iterion reads it on an interval. One connection is one
feed; a second feed is a second connection.

1. **Give the feed's own URL** as the connection's instance URL — the address
   that answers with the XML, not the site's home page: `https://example.org/feed/`,
   `https://example.org/blog/atom.xml`, `https://example.org/index.xml?lang=fr`.
   A site names it in its pages' `<link rel="alternate" type="application/rss+xml">`
   (or `application/atom+xml`). The URL is requested exactly as written — path,
   trailing slash and query string — and no redirect is followed, so give the
   final address: `https://`, and the trailing slash if the site serves its feed
   there (WordPress answers `/feed/` and redirects `/feed`). An address on a
   private network is refused.
2. **A public feed needs no credential**: choose *No credential*. A feed
   behind HTTP authentication takes *Username and password* (HTTP Basic), sent
   on every read. A feed whose access token sits in its URL keeps that token in
   the instance URL, which is stored as an address and not sealed as a
   credential: prefer Basic credentials where the publisher offers them.
3. **Grant `trigger` and `action`.** Each read spends the connection — its
   credential, or at least its address and its rate with the publisher — so
   polling needs the `action` grant as well as `trigger`, even for a public
   feed.

## What polling promises

- **The revisions it observes, not the history.** Every 15 minutes by default
  (5 at the least), iterion reads the whole document and delivers each entry
  once per revision it sees. An entry published and withdrawn between two
  reads is never seen; an entry edited twice between them is one event, its
  latest text; an entry that falls off a feed keeping only its last N items
  before a read is never seen.
- **An entry is its guid**, else its link, else the digest of its content —
  and its revision is the digest of what a reader sees (title, link, summary,
  content, dates, author, categories). An edited entry is delivered again as
  a revision; a publisher that changes an entry's guid makes it a new entry.
  An entry with neither guid nor link is a new entry whenever its text
  changes.
- **Enabling a poller replays nothing.** The first read records the entries
  already in the feed without launching anything; only what appears or
  changes afterwards is delivered.
- **What is read.** RSS 2.0 and Atom 1.0, in UTF-8 or windows-1252 (Latin-1
  and US-ASCII labels included); dates are rewritten as RFC 3339 in UTC when
  they parse. A feed above 10 000 entries or 256 levels of nesting is refused
  rather than cut (`ITERION_CONNECTOR_FEED_MAX_ITEMS`,
  `ITERION_CONNECTOR_FEED_MAX_DEPTH`).
