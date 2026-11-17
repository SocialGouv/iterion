# Connecting a Slack workspace

One Slack app per connection, so a deployment connected to two workspaces has
two apps and two signing secrets.

1. **Create the app.** <https://api.slack.com/apps> → **Create New App** →
   **From scratch**, pick the workspace.
2. **Give the bot its scopes.** *OAuth & Permissions* → **Scopes** →
   **Bot Token Scopes**: `chat:write` (the reply operation),
   `channels:read` and `groups:read` (the channel picker).
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

The app's team id is bound to the connection when the first delivery arrives;
if you connect a second workspace, give it its own connection.
