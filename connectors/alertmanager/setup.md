# Declaring the Alertmanager receiver

Alertmanager signs nothing: the receiver proves itself with a credential the
operator configures. This package declares a bearer token, which iterion mints
and shows once.

1. Open the `alertmanager.yml` of the instance that should deliver here.
2. Add a receiver whose `webhook_configs` entry names the delivery URL the
   wizard shows, and give it the token as its `authorization` credential:

   ```yaml
   receivers:
     - name: iterion-webhook
       webhook_configs:
         - url: https://iterion.example/api/webhooks/c/<ingress id>
           send_resolved: true
           http_config:
             authorization:
               type: Bearer
               credentials: <the token the wizard showed>
   ```

3. Route the alerts that should reach iterion to that receiver under
   `route.routes` — a `matchers` list narrows it, for example to
   `severity="critical"`.
4. Keep `send_resolved: true`, or the `resolved` trigger never fires. Reload
   Alertmanager (`POST /-/reload`, or restart it).
5. `group_by` decides what arrives as one body: every alert grouped together
   is one delivery, with `groupKey` naming the group.

The token is a plain bearer credential, so the receiver must reach iterion
over TLS.
