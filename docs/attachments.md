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

```iter
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
| `{{attachments.<name>.url}}`        | presigned URL — HMAC-signed local URL or SigV4 S3 URL depending on mode  |
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
| `<path>` | yes     | Everything up to the first `name=` / `mime=` token, so a path may contain spaces. Host-absolute, readable, and at most 50 MB. |
| `name`  | optional | The handle `/api/runs/{id}/attachments/{name}` serves. Defaults to the file's base name without its extension, sanitised to `[A-Za-z0-9_-]`. A name the run already carries is **never overwritten** — the directive is skipped with a warning, so a tool cannot clobber an operator's upload or an earlier iteration's deliverable. |
| `mime`  | optional | Stored as-is when well formed; otherwise sniffed from the extension, falling back to `application/octet-stream`. Types the browser would EXECUTE (html, xhtml, svg, xml, javascript) are downgraded to `application/octet-stream`: the serve route replies `Content-Disposition: inline` with no nosniff, and tool stdout is not a trusted channel. |

The runtime reads the bytes and persists them through the same
`WriteAttachment` path as an upload, so the result is an ordinary run
attachment: same storage layout, same serving route, same presigning.
One line per file; other stdout is left alone.

Failures are **non-fatal** — a missing or unreadable file is logged and
skipped, never enough to fail the tool node. The tool's own output is
its contract with the workflow; the directive is only how the bytes
reach a human.

To show it at a gate, return a descriptor from the same node and map it
in the edge's `with {}`:

```json
{"attachment": "final_video", "filename": "final.mp4",
 "mime": "video/mp4", "size": 57948692}
```

The gate's inbound payload renderer previews any value carrying an
`attachment` name plus one corroborating field. Images, audio and video
play inline; JSON, markdown and other text are shown in the gate (long
bodies start folded). A zip or other binary stays a download. Declaring
the field as `file` in the gate's input schema sharpens the reading
order; it is not required.

> This is the writing counterpart of `[iterion] preview_screenshot=`,
> which promotes a browser capture the same way.

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

Staged uploads live under `<store>/uploads/<upload_id>/` until the
launch promotes them to `<store>/runs/<run_id>/attachments/<name>/`.
Unreferenced uploads are reaped after one hour (`uploadStagingTTL`).

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
| Cloud (S3 / MinIO) | `attachments/<run_id>/<name>/<filename>` (S3 key); metadata reflected in the runs collection |

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

### CLI fallback (`claude_code`, `pi`, Kimi, Grok, legacy Codex)

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

## Cloud notes

- The runner pod reads the bytes through `blob.GetAttachment` (S3 /
  MinIO) when a node opens an attachment by URL or path. No shared
  filesystem is required.
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
