# Declaring the Forgejo webhook

1. In the repository (or the organisation, for every repo), open
   **Settings → Webhooks → Add webhook → Forgejo**.
2. Paste the delivery URL the wizard shows into **Target URL**.
3. Set **HTTP method** to POST and the **content type** to
   `application/json`.
4. Under **Trigger**, choose the events the subscription listens to —
   `issue_comment` for a comment on an issue or a pull request.
5. Paste the secret the wizard showed (it is displayed once) into
   **Secret**, so every delivery is signed.
6. **Add webhook**; Forgejo sends a first `ping` — it is ignored, the
   first real event is what launches the bot.
