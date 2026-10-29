# Attachments — file & image inputs

The `attachments:` block lets a workflow declare binary inputs (files,
images) the user provides at launch. Iterion uploads them once,
persists them under the run, and exposes them to nodes via
`{{attachments.<name>[.<sub>]}}` template references. Image
attachments are forwarded to vision-capable agents as native
multimodal `ContentBlock`s; arbitrary files are exposed as host
filesystem paths plus optional presigned URLs.

This document covers the DSL surface, the runtime semantics across
local / desktop / cloud, the upload protocol, and the security model.

> **Attachments can also arrive mid-run.** An operator answering a
> `human` gate can upload a file there — a `file`-typed schema field, or
> the always-available 📎 button. Those become ordinary run attachments
> through everything described below (same storage, same limits, same
> sandbox mount), just bound at answer time instead of launch time. See
> [human-in-the-loop.md](human-in-the-loop.md#handing-the-workflow-a-file).

## DSL

```iter fragment
attachments:
  logo: image
  spec: file
    description: "Spec PDF that grounds the review"
    accept_mime: ["application/pdf"]
    required: true
```

The block is valid at file-level (parallel to `vars:`) and
workflow-level (inside a `workflow <name>:` body). Attachment names
must be unique across file and workflow scopes; a duplicate emits
C050 and the later declaration is skipped.

| Field          | Required | Notes                                                                                       |
| -------------- | -------- | ------------------------------------------------------------------------------------------- |
| `<name>`       | yes      | Identifier referenced via `{{attachments.<name>}}`. Must not collide with a `vars:` entry.  |
| Type           | yes      | `file` or `image`. `image` enables multimodal forwarding to claw.                           |
| `description`  | optional | Surfaced in the Launch modal under the field label.                                         |
| `accept_mime`  | optional | List of `type/subtype` patterns (`*` glob allowed). Intersected with the server allowlist.  |
| `required`     | optional | Defaults to false. Required attachments block the Launch button until provided.             |

### Reference syntax

| Form                                | Resolves to                                                              |
| ----------------------------------- | ------------------------------------------------------------------------ |
| `{{attachments.<name>}}`            | host filesystem path (default; same as `.path`)                          |
| `{{attachments.<name>.path}}`       | host filesystem path                                                     |
| `{{attachments.<name>.url}}`        | presigned URL — HMAC-signed local URL or SigV4 S3 URL depending on mode; not found — the placeholder stays, `validate --exec` names it, the log says why — when no signer is wired or signing fails |
| `{{attachments.<name>.mime}}`       | sniffed MIME (e.g. `image/png`)                                          |
| `{{attachments.<name>.size}}`       | byte length as a decimal string                                          |
| `{{attachments.<name>.sha256}}`     | hex SHA-256 of the upload                                                |

Any other sub-field produces compile-time diagnostic `C054`. An
unknown attachment name produces `C053`.

### Diagnostics

| Code | Meaning                                                                |
| ---- | ---------------------------------------------------------------------- |
| C050 | attachment name declared more than once                                |
| C051 | attachment name collides with a declared `vars:` entry                 |
| C052 | `accept_mime` entry is not in `type/subtype` form                      |
| C053 | `{{attachments.X}}` references an undeclared attachment                |
| C054 | unknown sub-field after `attachments.<name>.` (only `path`, `url`, `mime`, `size`, `sha256` are valid) |

## A tool node hands over a file it produced

The three sources above all bring bytes **in** from a person: the launch
form, a `file`-typed gate field, the 📎 button. A workflow that
*generates* a deliverable — a rendered video, an audio mix, a chart, a
PDF — had no way to put it in front of a reviewer, because a human gate
previews a `file` value by fetching
`GET /api/runs/{id}/attachments/{name}` and the path a tool knows is a
host or bind-mount path the browser cannot reach.

A tool node declares one by printing a directive on stdout:

```sh
echo "[iterion] attachment=$PWD/exports/final.mp4 name=final_video mime=video/mp4"
```

| Token   | Required | Notes                                                                    |
| ------- | -------- | ------------------------------------------------------------------------ |
| `<path>` | yes     | Everything up to the first `name=` / `mime=` / `required=` token, so a path may contain spaces. Host-absolute, readable, and at most 50 MB. |
| `name`  | optional | The handle `/api/runs/{id}/attachments/{name}` serves. Defaults to the file's base name without its extension, sanitised to `[A-Za-z0-9_-]`. A name the run already carries is **never overwritten** — the directive is skipped with a warning, so a tool cannot clobber an operator's upload or an earlier iteration's deliverable. |
| `mime`  | optional | Stored as-is when well formed; otherwise sniffed from the extension, falling back to `application/octet-stream`. Types the browser would EXECUTE (html, xhtml, svg, xml, javascript) are downgraded to `application/octet-stream`: the serve route replies `Content-Disposition: inline` with no nosniff, and tool stdout is not a trusted channel. |
| `required` | optional | `required=true` makes the loss of THIS file the node's failure instead of an event beside its success. Off by default. The usual boolean spellings answer; any other value (`yes`, a typo, an empty value) reads as **true** — the token is only ever typed to ask for the guarantee, so a spelling no parser knows must not quietly cancel it. |

The runtime reads the bytes and persists them through the same
`WriteAttachment` path as an upload, so the result is an ordinary run
attachment: same storage layout, same serving route, same presigning.
One line per file; other stdout is left alone.

### It shows up in the run summary

A published file is also announced as a **headline result link**: the run
summary lists `Deliverable → …/attachments/<name>` beside the pull
requests the run opened, so a reader finds what it produced without
opening a gate or the artifacts drawer. The engine emits one
`preview_url_available` event per published file, carrying
`kind=deliverable` — a link-only kind, never embedded in the Browser
pane — plus `scope=external` and `source=tool-attachment`, which is what
tells it apart from a `[iterion] preview_url=` line a tool printed
itself.

The address is the existing serving route, so the link grants exactly the
rights the run does and no more: the route resolves the handle through
the run document under the caller's own tenant, so a member of another
team gets a 404 from the link just as from the run. The companion
`/api/runs/{id}/attachments/{name}/url` mints an HMAC-signed URL instead
— a bearer address any user agent can follow until it expires, which is
a deliberate sharing gesture and not what the summary link is.

It is **absolute**, built from `ITERION_PUBLIC_URL`, and when that origin
is unset or unusable there is **no link at all** rather than a
root-relative one: the studio reaches its own API through a base that is
`/x/<id>/api` inside a scoped pane and a different origin in the desktop
app, so a published `/api/runs/…` would address the wrong server. The
file is published either way — `run_attachment_published` still carries
its handle, which is what a gate reads — and the reason an origin was
refused is logged once per run, the same place `run.url` reports it.

Failures are **non-fatal by default** — a missing or unreadable file is
skipped, never enough to fail the tool node. The tool's own output is
its contract with the workflow; the directive is only how the bytes
reach a human.

A workflow whose POINT is the file says so with `required=true`, and
then the loss fails the node:

```sh
echo "[iterion] attachment=$PWD/report.md mime=text/markdown required=true"
```

The node ends with `required attachment=<path> was not delivered to the
run (<reason>)`, after the `run_attachment_lost` event is recorded — the
failure replaces nothing, it is added. The guarantee is about the LOSS,
so a required file that arrives leaves the node exactly as green as
before, and the token changes nothing for any node that does not carry
it. Three boundaries are deliberate:

- A recipe that FAILED is judged by its own exit status: the files it
  did not finish writing are that failure's consequence, and no
  delivery verdict is computed for it.
- A node with a `postcondition:` fails on the loss too, before the
  recovery rungs — the postcondition answers about the world the recipe
  acted on, while the loss is about the run's own store, and no rung can
  mend it by re-running a command that already exited 0.
- A `postcondition:` body, and an `action:` node's vendor payload, still
  publish what they declare but can never FAIL the node: a check is the
  node's oracle rather than its producer, and a response body the vendor
  controls must not be able to end a run.

`required=` therefore hands an author a loud failure in place of a
silent loss — and, like every token on this line, it is read from a
stream the author does not fully own (tool stdout carries fetched pages,
catted files and model-written text). Opting in accepts that a line
planted there can fail the node; it cannot corrupt what the node
delivered.

They are not **silent**, though. Every exit that does not publish emits
a `run_attachment_lost` event on the run, carrying the declared `path`,
the `reason` (`unreadable`, `not_a_file`, `over_cap`, `name_taken`,
`store_write_failed`), a human `detail`, and `required` — always both
ways, so a reader filtering the losses that failed a node cannot read an
absent field as a `false`. Without it the only trace
was a warning in the runner's log — and on a sandbox pod that is the
one place the operator cannot read, so a run could lose its whole
deliverable and look like it succeeded (#2668). The sibling
`[iterion] preview_screenshot=` verb reports its losses the same way;
the event names which directive it came from.

To show it at a gate, return a descriptor from the same node and map it
in the edge's `with {}`:

```json
{"attachment": "final_video", "filename": "final.mp4",
 "mime": "video/mp4", "size": 57948692}
```

The gate's inbound payload renderer previews any value carrying an
`attachment` name plus one corroborating field. Images, audio and video
play inline; JSON, markdown and other text (`.txt`, `.csv`, `.yaml`,
`.log`, …) are shown in the gate (long bodies start folded). `.xml`,
`.html`, `.svg` and `.js` stay a download: `neutralizeActiveMIME`
downgrades them so the attachment route cannot serve executable bytes
inline, and the studio preview classifier matches that set. A zip or
other binary stays a download. Declaring
the field as `file` in the gate's input schema sharpens the reading
order; it is not required.

> This is the writing counterpart of `[iterion] preview_screenshot=`,
> which promotes a browser capture the same way.

## A node declares the files it produces

The directive above is a line the tool PRINTS. A node can declare the same
files up front instead, so the contract is readable without running
anything:

```iter fragment
tool export_report:
  command: `make report`
  deliverables: ["report.md", "out/annex.html"]
```

A declared deliverable is a **workspace-relative** path, and its **base
name** is the handle the run serves it under: `out/annex.html` is served as
`annex.html`, so a download address carries the file name the author wrote.
`agent` and `judge` nodes accept the property too — they run tools in the
same workspace. A `human` node's file comes from the operator's own upload
and a `compute` node writes none, so there the parser refuses the property
by name instead of dropping the declaration in silence.

Two shapes are refused where the declaration is written, because no run
could keep the promise:

| Code | What it refuses                                                      |
| ---- | -------------------------------------------------------------------- |
| C317 | An entry that is not a workspace-relative file path: absolute (`/tmp/report.md`), escaping upwards (`../report.md`), naming a directory (`out/`), or empty. A `{{…}}` reference or a `${…}` form does not excuse the shape — no rendering makes `/tmp/report.md` relative — so a templated entry is held to the same rules; what it renders TO is the collector's to refuse, where the path is resolved. |
| C318 | Two entries of one node sharing a base name (`out/report.md` and `tmp/report.md`): both would be served as `report.md`, and the run store refuses every one after the first rather than overwrite it. Said once per node, each shared name with its entries. The handle is per node, so two different nodes may each declare `report.md`. |

The declaration has ONE reader, `ir.NodeDeliverables`, and the handle has
one definition, `ir.DeliverableName`: the compiler that judges a
declaration and the collector that uploads the bytes cannot disagree about
which file is served under which name. The collection itself — the
node-end upload into the run's file store, and the download route that
serves a declared name — is the next slice of iterion#2690; a tool that
must hand a file to a human today prints the directive above.

## Upload protocol

The Launch modal uploads each attachment immediately on selection via
`POST /api/runs/uploads` (`multipart/form-data`, single `file` field).
The server returns an `upload_id` that the launch payload references:

```http
POST /api/runs/uploads
Content-Type: multipart/form-data; boundary=...

# multipart body with `file` field

{
  "upload_id": "up_1717169012_aabbccdd",
  "original_filename": "logo.png",
  "mime": "image/png",
  "size": 42184,
  "sha256": "…"
}
```

```http
POST /api/runs
Content-Type: application/json

{
  "file_path": "/path/to/workflow.bot",
  "attachments": { "logo": "up_1717169012_aabbccdd" }
}
```

Where a staged upload waits is the run store's business — the
`store.StagingStore` seam, a capability a store has or has not, like the
run-files area. A local store keeps it under `<store>/uploads/<upload_id>/`
(the bytes under the original filename, a `meta.json` sidecar) until the
launch promotes it to `<store>/runs/<run_id>/attachments/<name>/`. The
cloud store has no disk: the record lives in the `staged_uploads`
collection, stamped with the tenant and the user that staged it, and the
bytes in the bucket under `uploads/<upload_id>/<filename>`; the launch
promotes them to `attachments/<run_id>/…`. A promotion from another
tenant does not find the upload, and nothing presigns one. A store that
stages nothing answers the upload with a typed 503. Unreferenced uploads
are reaped after one hour (`uploadStagingTTL`), on every server replica.

### Limits

The upload handler enforces four limits. In local editor mode,
`iterion studio` exposes flags for these settings; the cloud
`iterion server` command currently exposes only its server flags
(port/bind/dir/store-dir/config), so upload limits there use the
server configuration defaults unless an embedder wires explicit
`server.Config` values.

| `iterion studio` flag       | Default (web/cloud) | Default (desktop) |
| --------------------------- | -------------------- | ----------------- |
| `--max-upload-size`         | 50 MB                | 1 GB              |
| `--max-total-upload-size`   | 5 × max-upload-size  | 5 × max-upload-size |
| `--max-uploads-per-run`     | 20                   | 20                |
| `--allow-upload-mime`       | safe defaults        | safe defaults     |

The default MIME allowlist covers `image/{png,jpeg,gif,webp}`,
`application/{pdf,json,zip,gzip,x-tar}`, `text/{plain,markdown,csv}`,
`application/yaml`, and `application/octet-stream` (the fallback for
files whose type can't be sniffed). The `GET /api/server/info` endpoint returns
the resolved limits so the SPA can surface them before any byte
leaves the browser.

Errors are mapped to standard codes:

| Status | Cause                                                          |
| ------ | -------------------------------------------------------------- |
| 413    | upload exceeds the configured per-file or cumulative size limit |
| 415    | sniffed MIME is not in the configured upload MIME allowlist     |
| 422    | declared name not present in the workflow's `attachments:`     |
| 409    | more attachments referenced than the configured per-run limit   |

## Storage layout

| Mode         | Layout                                                     |
| ------------ | ---------------------------------------------------------- |
| Local / desktop | `<store>/runs/<run_id>/attachments/<name>/<filename>` plus a sidecar `meta.json` |
| Cloud (S3-compatible store) | `attachments/<run_id>/<name>/<filename>` (S3 key); metadata reflected in the runs collection |

The metadata struct (`AttachmentRecord`) carries `name`,
`original_filename`, `mime`, `size`, `sha256`, `created_at`, and a
`storage_ref` pointing at the canonical key. It is persisted on
`Run.Attachments` so resume reads the same data the original launch
saw — there is no special-case retry path.

### Presigned URLs

`{{attachments.<name>.url}}` produces:

- Local / desktop: `/api/runs/<id>/attachments/<name>?exp=…&sig=…`,
  HMAC-signed with a per-store random key. Default TTL 10 minutes.
  The signing key lives at `<store>/.attachment-signing-key`.
- Cloud: a SigV4-signed S3 GET URL valid for the same TTL.

The bytes endpoint also accepts safe-Origin browser callers (no
signature) so the studio SPA can read attachments without minting a
URL first.

## Runtime semantics

When the engine starts a run, `loadAttachmentInfos` reads
`Run.Attachments` and builds the per-template snapshot consumed by
node prompts and tool commands. The path is the absolute host path
in local mode and `/run/iterion/attachments/<name>/<filename>` inside
the sandbox (read-only bind mount).

### Multimodal forwarding (claw)

For agent nodes whose backend is `claw`, the executor:

1. Pre-scans the resolved user prompt for `{{attachments.X}}` (or
   `.path`) references where `X` is declared as `image`.
2. Splits the prompt into alternating text and image content blocks.
3. For each image block, base64-inlines bytes ≤ 5 MB or falls back to
   a presigned-URL block for larger files.

The blocks land on the Anthropic Messages API as native vision input —
no tool call needed.

### CLI fallback (`claude_code`, Codex, `pi`, Kimi, Grok)

Every CLI-based backend follows the path fallback rather than receiving claw's
inline multimodal blocks. The
executor:

- Interpolates `{{attachments.X}}` to the host file path as usual.
- Auto-enables the `read_image` tool on the node so the agent can
  fetch the bytes itself.

This allowlist entry does not install a tool into a delegated CLI. The target
agent must actually expose `read_image` (or use its own native image/file reader)
and its selected model must support vision. In particular, pi has a `read` tool
rather than `read_image`, while the generic Kimi and Grok adapters do not add any
iterion image tool. Treat path delivery as the portable contract and verify the
target CLI's image support.

### Sandbox

When a sandbox is active, the engine appends a read-only bind mount
of the run's `attachments/` directory under
`/run/iterion/attachments`, and `{{attachments.<name>.path}}` resolves
to that container path rather than the host one — the host path does
not exist inside the container, and handing it to an agent produces a
plausible-looking path that fails to open. The mount is read-only by
construction: a malicious agent cannot corrupt the run store.

A driver with no host filesystem (kubernetes) cannot bind the directory;
it receives it as a **seed** instead (`sandbox.RunInfo.SeedDirs`): the
pod declares an `emptyDir` at `/run/iterion/attachments`, the directory
streams in with the workspace's tar pipeline after the pod is ready, and
the copied files are made read-only (the volume itself stays writable —
an `emptyDir` is root-owned and the sandbox user cannot change its
mode). The run's stream says so (`sandbox_attachments_copied`), and
`{{attachments.<name>.path}}` resolves to the same container path as on
docker. A driver that can neither bind nor copy refuses a run that holds
attachments (`ATTACHMENTS_UNDELIVERABLE`); a node that names a path the
run cannot hand it fails before it runs (`ATTACHMENT_PATH_UNAVAILABLE`) —
except an image attachment referenced with the plain `{{attachments.<name>}}`
or `.path` spelling in the user prompt of an agent or judge whose every
route is `claw`: that executor sends an image block, by URL when no copy
exists, and needs no path. A `{{!…}}` spelling, a user prompt that is also
the node's system or interaction prompt, and a router's prompt render the
path as text, and are admitted on it.

`meta.json` is nobody's filename: the filesystem layout keeps each
attachment's record under that name beside its bytes, so a file under it
would be read back as the record. Both stores refuse it, typed, whoever
writes the attachment — an upload (a 400 at the door), a node's
attachment directive, a bundle default, an answered file.

## Cloud notes

- The runner pod **materialises** the run's attachments before the
  sandbox: the engine lists them on the Mongo store, streams each from
  the blob backend into a directory of the run's own (`<name>/<filename>`,
  read-only files) and removes it with the sandbox. That directory is
  what the kubernetes driver seeds into the pod, and what the nodes open
  by path when the run executes in the runner's own process; the
  presigned URL (`{{attachments.<name>.url}}`) serves either way. A fetch
  that fails parks the run resumable, typed `ATTACHMENTS_UNFETCHABLE`,
  instead of running it without its files; a redelivery fetches again.
  No shared filesystem is required. A cloud launch promotes the staged
  uploads it names on the server, between the run row and the queue
  message; a promotion that fails fails the launch (`LAUNCH_FAILED`).
- Upload limits are advisory at the SPA level; the server pod
  re-validates each upload with its compiled server configuration.
  Today the cloud command has no upload-limit surface in `pkg/config`
  or `charts/iterion`, so it uses code defaults unless a future
  deployment wrapper or embedder passes explicit values.

## Authoring tips

- Prefer the `image` type whenever the file is meant for an LLM's
  vision input — you get free multimodal forwarding to claw without
  changing the node definition.
- For large PDFs or archives, declare them as `file` and stream them
  to a tool node (`cat`, `unzip`, …) rather than interpolating the
  whole content into a prompt.
- Use `accept_mime` to lock down what users can upload. The Launch
  modal renders the constraint as a `<input accept="…">` hint AND
  validates client-side before any byte leaves the browser.
- Set `required: true` for attachments the workflow cannot run
  without — the Launch button stays disabled until provided.
