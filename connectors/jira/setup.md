# Connecting a Jira site

One webhook per Jira site, so a deployment connected to two sites has two
connections and two secrets. Jira Cloud signs every delivery it sends to a
webhook that has a secret.

1. **Create the webhook.** As a Jira administrator, open **Settings → System →
   WebHooks** and choose **Create a WebHook** (a project administrator can add
   one from the project's **Project settings → WebHooks** instead). Paste the
   delivery URL the wizard shows into the URL field.
2. **Select the events.** Under **Events**, tick the ones your subscription
   listens to: **Issue → created**, **Issue → updated**, **Comment → created**,
   **Comment → updated**, **Comment → deleted**. Leave the JQL field empty
   unless you want Jira to filter before it sends; the connection's own filters
   run on the deliveries that arrive.
3. **Paste the secret.** In the webhook's **Secret** field, paste the value the
   wizard showed (it is displayed once). Iterion minted it, so Jira cannot
   generate it for you. Every delivery is then signed with
   `X-Hub-Signature: sha256=<hex>`, an HMAC-SHA256 of the raw body.
4. **Save.** Jira sends no verification handshake: the first real event is what
   proves the hook, and a misconfigured one simply delivers nothing.

## Registering through the REST API instead

Jira Cloud can also register a webhook dynamically:
`POST https://<site>.atlassian.net/rest/api/3/webhook` with the same URL and
secret, `webhooks[].events` naming the event strings the package declares
(`jira:issue_created`, `jira:issue_updated`, `comment_created`, and so on).
Dynamic webhooks belong to an OAuth 2.0 (3LO) app and expire, so the app must
keep refreshing them; a plain webhook from **System → WebHooks** does not.

This package declares no provisioning operation: the webhook is created by
hand, once per connection.

## The action credential

The connection's credential is spent by action operations and by the JQL
poller below — the webhook triggers do not need it. Jira authenticates those
with **Basic** auth using the
Atlassian account's email as the username and an API token you create at
<https://id.atlassian.com/manage-profile/security/api-tokens> as the password,
or with an OAuth 2.0 bearer token.

## Polling instead of a webhook

The **Issue changed (polled)** trigger needs no webhook: iterion asks Jira's
search for the issues that changed, every five minutes by default (one at the
least). Use it when you cannot add a webhook to the site, or want one bot per
JQL rather than per site.

1. **The connection spends its credential.** Each tick calls
   `GET /rest/api/3/search/jql` with the connection's Basic credentials (email
   and API token — the recommended credential for polling), so the connection
   needs the `action` grant as well as `trigger`. An OAuth 2.0 (3LO) token
   works only against `https://api.atlassian.com/ex/jira/<cloudid>` as the
   connection's URL, not the site's, and expires with no refresh here: the
   poller then says so on its subscriptions. The account sees
   what it may browse: an issue outside its permissions is never delivered.
2. **Write the JQL filter**, without `ORDER BY` — `project = OPS`, `project =
   OPS AND labels = sources`. It must be bounded (Jira refuses a search with no
   restriction). The poller wraps it as `(<your JQL>) AND updated >=
   <checkpoint> ORDER BY updated ASC`, the checkpoint in epoch milliseconds,
   which Jira reads the same whatever the account's profile time zone.

What polling promises: the revisions it observes, not the history. An issue is
delivered once per `updated` value a tick sees, so two edits between two
ticks are one event, and the search carries neither the changelog nor who made
the change (no `actor`, no `updated_fields`, which the webhook triggers have).
Each tick re-reads one minute before its checkpoint, so edits landing at the
same instant are not lost across ticks; an edit that becomes searchable more
than a minute late — Jira's index is eventually consistent — is missed.
Enabling a poller replays nothing: its first scan records the issues the JQL
already matches without launching. A scan reads at most 20 pages (of up to
100 issues each); one that stops there is incomplete and does not move the
checkpoint, so keep the JQL narrow enough to fit.
