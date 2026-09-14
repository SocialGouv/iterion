# Report access selection

The input `tenant_id` is the authenticated tenant identity. A result is an
authorization decision: the caller serves the report matching every returned
grant ID. Grants may belong to several tenants.

A grant is eligible exactly when it belongs to the requesting tenant and its
expiry is strictly later than `now`. Equality means expired. Return eligible
IDs in input order; neither mutate the input nor deduplicate equal IDs.
Empty input returns an empty list. `now` and `expires_at` use integer seconds.

Run the standard-library checks with:

    python3 -m unittest discover -s diagnostics/revi-probe -v
