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

The connection's credential is spent by action operations — the webhook
triggers do not need it. Jira authenticates those with **Basic** auth using the
Atlassian account's email as the username and an API token you create at
<https://id.atlassian.com/manage-profile/security/api-tokens> as the password,
or with an OAuth 2.0 bearer token.

## Polling instead of a webhook

The **Issue changed (polled)** trigger needs no webhook: iterion searches the
site on an interval. Plugging it shows its own notes instead of these.
