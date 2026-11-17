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
5. **Check the alert rules.** An issue webhook fires only for issues an alert
   rule covers, and a metric alert only when a metric alert rule crosses its
   threshold — with no rule, no delivery ever arrives.
6. **Send a test alert** from an alert rule, or resolve an issue that has one:
   the delivery is signed with the Client Secret and the first one binds the
   organization (its installation uuid) to the connection.

The integration's API token is separate from its Client Secret: the secret
proves the deliveries Sentry sends you, the token is what iterion sends to
Sentry. Both come from the same page.
