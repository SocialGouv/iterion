# Declaring the Forgejo webhook

The loop guard below drops the comments of the account iterion comments with,
whose login the package names `iterion-bot`. On an instance where that account
answers as another name, adjust the guard: a comment by the bot would otherwise
trigger its own next run.

1. In the repository (or the organisation, for every repo), open
   **Settings → Webhooks → Add webhook → Forgejo**.
2. Paste the delivery URL the wizard shows into **Target URL**.
3. Set **HTTP method** to POST and the **content type** to
   `application/json`.
4. Under **Trigger**, choose the events the subscription listens to —
   `issue_comment` for a comment on an issue or a pull request.
5. Paste the secret the wizard showed (it is displayed once) into
   **Secret**, so every delivery is signed.
6. **Add webhook**, then use **Test Delivery** if you want one: it arrives as
   an event no trigger declares, so it is answered and dropped.
