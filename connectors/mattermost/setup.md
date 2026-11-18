# Connecting a Mattermost team

An outgoing webhook belongs to one team — or to the whole instance when you
create it from the System Console, in which case every team it fires for needs
its own connection.

1. **Create the hook.** In Mattermost, open the channel menu → **Integrations**
   → **Outgoing Webhooks** → **Add Outgoing Webhook** (a system-wide one lives
   under **System Console → Integrations → Outgoing Webhooks**).
2. **Name it and pick its channels.** A hook with no channel fires in every
   channel of its team; set a **Trigger Word** if the bot should only hear
   messages that address it — that word is also your loop control, since the
   payload carries no marker for a message a bot posted.
3. **Set the callback URL** to the delivery URL the wizard shows.
4. **Set the content type to `application/json`.** The hook form offers the
   form encoding too, and this package reads only JSON: a form-encoded
   delivery is refused.
5. **Copy the token.** Mattermost generates it: the hook page shows it with
   **Copy Token** (and **Regenerate Token** rolls it). Paste that value into
   the connection's secret field — Mattermost owns it, so iterion cannot mint
   it.
6. **Save**, then post a message in one of the hook's channels: the delivery
   arrives signed by nothing but that token, which is exactly what
   authenticates it.

The ingress asks for the hook's **team id** when you create it: read it with
`GET /api/v4/teams/name/<team-name>` (`id` in the answer). Two teams need two
connections. A message carries no `root_id`, so a reply and a
top-level post are the same event and the channel is the conversation.
