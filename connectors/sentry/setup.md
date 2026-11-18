# Connecting a Sentry organization

One internal integration per organization: an integration is installed on the
organization that creates it, and its webhook only carries that organization's
events. Two organizations need two connections.

1. **Create the integration.** In Sentry, open **Settings → Developer
   Settings → Custom Integrations → Create New Integration → Internal
   Integration**.
2. **Give it a name and the delivery URL** the wizard shows as **Webhook
   URL**. Sentry requires the URL when the integration is created.
3. **Enable the webhooks you need** in the integration's **Webhooks** section:
   *issue* for the issue-alert trigger, *metric alert* for the metric-alert
   one.
4. **Save**, then copy the **Client Secret** from the integration's page and
   paste it into the connection's secret field. Sentry generated it; iterion
   cannot.
5. **Know what each resource means.** The `issue` webhook follows the issue
   lifecycle of every project the integration is installed on — it is not an
   alert rule firing, and it needs no rule. The `metric alert` webhook is the
   one that follows a metric alert rule crossing its
   threshold — with no rule, no delivery ever arrives.
6. **Send one delivery**: resolve an issue for the `issue` webhook, or trip a
   metric alert rule for the `metric alert` one. It is signed with the Client
   Secret — that secret, not an installation id, is what proves the sender
   here (see `triggers.yaml`).

The integration's API token is separate from its Client Secret: the secret
proves the deliveries Sentry sends you, the token is what iterion sends to
Sentry. Both come from the same page.
