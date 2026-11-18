# Connecting a Slack workspace

One Slack app per connection, so a deployment connected to two workspaces has
two apps and two signing secrets.

1. **Create the app.** <https://api.slack.com/apps> → **Create New App** →
   **From scratch**, pick the workspace.
2. **Give the bot its scopes.** *OAuth & Permissions* → **Scopes** →
   **Bot Token Scopes**: `chat:write` (the reply operation), `channels:read`
   and `groups:read` (the channel picker), and one per event you subscribe to
   below — `channels:history` for `message.channels`, `app_mentions:read` for
   `app_mention`, `reactions:read` for `reaction_added`. Without them Slack
   accepts the subscription and delivers nothing.
3. **Subscribe to the events.** *Event Subscriptions* → **Enable Events** →
   paste the delivery URL the wizard shows as **Request URL**. Slack answers
   immediately: it asks the endpoint to echo a challenge, which the package
   does by itself when the signature is valid. Then under **Subscribe to bot
   events** add `message.channels`, `app_mention` and `reaction_added` — the
   ones the trigger you picked listens to.
4. **Install it.** *Install App* → **Install to workspace**, then invite the
   bot to the channels it should see (`/invite @your-bot`). Slack sends
   nothing from a channel the app is not in.
5. **Paste the signing secret.** *Basic Information* → **App Credentials** →
   **Signing Secret** → **Show**, and paste it into the connection's secret
   field. Slack generated it, so iteration cannot: this is the one value the
   wizard asks you to bring.

The ingress asks for the app's **team id** when you create it: it is the
`T…` segment of the workspace URL (`app.slack.com/client/T024BE7LD/…`). One
workspace per connection — a second workspace needs its own app and its own
connection.
