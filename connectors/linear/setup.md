# Declaring the Linear webhook

Linear's hook API is GraphQL, which iterion's executor does not speak yet, so
the webhook is declared by hand: iterion mints the signing secret, you paste it
into Linear.

1. In Linear, open **Settings → API → Webhooks → New webhook**.
2. Paste the delivery URL the wizard shows into **URL**.
3. Under **Data change events**, tick the entity types the subscription
   listens to — **Issues** and **Comments** for the triggers in this package.
4. Paste the signing secret the wizard shows (it is displayed once) into the
   webhook's **Signing secret** field. Linear signs every payload with it.
5. **Create webhook**. Linear sends nothing until an event fires; a delivery
   older than a minute is refused, and a payload from another organisation is
   refused because the connection is bound to yours.
