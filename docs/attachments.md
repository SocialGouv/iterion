# Attachments — file & image inputs at launch time

The `attachments:` block lets a workflow declare binary inputs (files,
images) the user provides at launch. Iterion uploads them once,
persists them under the run, and exposes them to nodes via
`{{attachments.<name>[.<sub>]}}` template references. Image
attachments are forwarded to vision-capable agents as native
multimodal `ContentBlock`s; arbitrary files are exposed as host
filesystem paths plus optional presigned URLs.

This document covers the DSL surface, the runtime semantics across
local / desktop / cloud, the upload protocol, and the security model.

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
workflow-level (inside a `workflow <name>:` body). Workflow-level
declarations override file-level ones with the same name.

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
  "file_path": "/path/to/workflow.iter",
  "attachments": { "logo": "up_1717169012_aabbccdd" }
}
```

Staged uploads live under `<store>/uploads/<upload_id>/` until the
launch promotes them to `<store>/runs/<run_id>/attachments/<name>/`.
Unreferenced uploads are reaped after one hour (`uploadStagingTTL`).

### Limits

The server applies four limits, each adjustable via flags:

| Flag                       | Default (web/cloud) | Default (desktop) |
| -------------------------- | -------------------- | ----------------- |
| `--max-upload-size`        | 50 MB                | 1 GB              |
| `--max-total-upload-size`  | 5 × max-upload-size  | 5 × max-upload-size |
| `--max-uploads-per-run`    | 20                   | 20                |
| `--allow-upload-mime`      | safe defaults        | safe defaults     |

The default MIME allowlist covers `image/{png,jpeg,gif,webp}`,
`application/{pdf,json,zip,gzip,x-tar}`, `text/{plain,markdown,csv}`,
and `application/yaml`. The `GET /api/server/info` endpoint returns
the resolved limits so the SPA can surface them before any byte
leaves the browser.

Errors are mapped to standard codes:

| Status | Cause                                                          |
| ------ | -------------------------------------------------------------- |
| 413    | upload exceeds `--max-upload-size` (per file or cumulative)    |
| 415    | sniffed MIME not in `--allow-upload-mime`                      |
| 422    | declared name not present in the workflow's `attachments:`     |
| 409    | more attachments referenced than `--max-uploads-per-run`       |

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
signature) so the editor SPA can read attachments without minting a
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

### CLI fallback (claude_code, codex)

CLI-based backends cannot accept inline images on stdin. The
executor:

- Interpolates `{{attachments.X}}` to the host file path as usual.
- Auto-enables the `read_image` tool on the node so the agent can
  fetch the bytes itself.

The agent is expected to call `read_image(path)` to load the image
through its own vision pipeline.

### Sandbox

When a sandbox is active, the engine appends a read-only bind mount
of the run's `attachments/` directory under
`/run/iterion/attachments`. Path references resolved inside the
sandbox point there. The mount is read-only by construction: a
malicious agent cannot corrupt the run store.

## Cloud notes

- The runner pod reads the bytes through `blob.GetAttachment` (S3 /
  MinIO) when a node opens an attachment by URL or path. No shared
  filesystem is required.
- Upload limits are advisory at the SPA level; the server pod
  re-validates each upload against its config (set via
  `ITERION_MAX_UPLOAD_SIZE` etc. in `charts/iterion/values.yaml`).

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
