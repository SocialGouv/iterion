# Polling a Jira site

The **Issue changed (polled)** trigger needs no webhook: iterion asks Jira's
search for the issues that changed, every five minutes by default (one at the
least). Use it when you cannot add a webhook to the site, or want one bot per
JQL rather than per site. Nothing is configured at Jira's side.

1. **Give the site's URL** as the connection's instance URL —
   `https://<site>.atlassian.net` for Jira Cloud.
2. **Use Basic credentials**, the recommended credential for polling: the
   Atlassian account's email as the username and an API token you create at
   <https://id.atlassian.com/manage-profile/security/api-tokens> as the
   password. Each tick calls `GET /rest/api/3/search/jql` with them. An OAuth
   2.0 (3LO) bearer token works only against
   `https://api.atlassian.com/ex/jira/<cloudid>` as the connection's URL, not
   the site's, and expires with no refresh here: the poller then says so on
   its subscriptions. The account sees what it may browse: an issue outside
   its permissions is never delivered.
3. **Grant `trigger` and `action`.** Each tick spends the connection's
   credential, so polling needs the `action` grant as well as `trigger`.
4. **Write the JQL filter**, without `ORDER BY` — `project = OPS`, `project =
   OPS AND labels = sources`. It must be bounded (Jira refuses a search with no
   restriction). The poller wraps it as `(<your JQL>) AND updated >=
   <checkpoint> ORDER BY updated ASC`, the checkpoint in epoch milliseconds,
   which Jira reads the same whatever the account's profile time zone.

## What polling promises

- **The revisions it observes, not the history.** An issue is delivered once
  per `updated` value a tick sees, so two edits between two ticks are one
  event, and the search carries neither the changelog nor who made the change
  (no `actor`, no `updated_fields`, which the webhook triggers have).
- **Late edits.** Each tick re-reads one minute before its checkpoint, so edits
  landing at the same instant are not lost across ticks; an edit that becomes
  searchable more than a minute late — Jira's index is eventually consistent —
  is missed.
- **Enabling a poller replays nothing.** Its first scan records the issues the
  JQL already matches without launching, reading the last day at most, so a
  backfill reaches no further back.
- **A bounded scan.** A scan reads at most 20 pages (of up to 100 issues
  each); one that stops there is incomplete and does not move the checkpoint,
  so keep the JQL narrow enough to fit.
