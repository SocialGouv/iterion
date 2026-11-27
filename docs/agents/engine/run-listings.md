# Run listings

The run listings (`/api/runs`, `/api/admin/runs`) ask Mongo one indexed
question per page (`store.RunWindowListingStore`, #3822): status, workflow,
`updated_at >= since`, a tenant set and the `(created_at, _id)` keyset,
newest first through `created_id_desc` (`tenant_created_id_desc` for a team,
`tenant_status_created_id_desc` / `tenant_workflow_created_id_desc` for a
team's status or workflow, #3831). A page no index serves in that order —
a `since` page, or a status or workflow page no tenant leads (the
cross-tenant view, or a tenant set naming the tenant-less runs) — reads in
finds of at most 100 documents so the planner's trial cannot crown a scan
that then walks the collection. Every other page is one find: a plan another
tenant's call left in the plan cache ran the later batches of a batched page.
Known gap, tracked as a follow-up: a tenant set of more than 200 members is
read in one find yet can still walk, because Mongo merges a `tenant_id $in`
in the listing order only up to `internalQueryMaxScansToExplode` (200)
index scans.
`EnsureSchema` creates indexes and never drops one, so a deployment that
predates #3831 keeps `tenant_status_created`, which
`tenant_status_created_id_desc` supersedes. Kept, it costs about 1 ms more
at p50 on every status update and one more candidate in each planner trial.
Once every replica and runner pool runs this release, the operator checks
that `db.runs.getIndexes()` lists `tenant_status_created_id_desc`, then runs
`db.runs.dropIndex("tenant_status_created")`; no query hints it by name.
Each tenant-led index also lengthens the trial of a query that reads a
whole tenant without reaching 101 results, such as the admin cutover scans. Those predicates read the persisted document, which is safe because both formats store every listing key as plain BSON outside the exact codec's paths — same
type, same bytes (`TestRunListingKeysEncodeIdenticallyInBothValueFormats`).
Each matched document then crosses the decode `LoadRun` uses; one that does
not decode is reported per id and never fails the page. Repo, bundle and node
filters stay in Go, and while one is set the limit is not pushed down.
Equivalence with the id-by-id scan: `TestRunListingWindowNativeMongo`.
