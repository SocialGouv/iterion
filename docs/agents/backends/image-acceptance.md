# Image acceptance — a route that takes no image refuses it

Read it when a node fails with "not a multimodal model", an image tool
answers that the route does not accept images, or you declare `image_input`
for a gateway id (#3707).

Whether a route takes images at all is the policy's `Acceptance`, resolved
per generation (`routeImageInput`, then `tool.ImageAcceptanceFor`). The
answer is the catalog's `image_input` flag (`modelspecs.Spec.ImageInput`).
A gateway route reads it from the catalog rung that answers the id, in the
usual order: the host's forwarded record in the sandbox, the run's feed (a
LiteLLM listing's `supports_vision`, a declared or exec entry's
`image_input`), the operator table (`"image_input": false`), then the
models.dev provider row (`modalities.input`), live then snapshot. That
rung answers whole, so it answers the image flag too. A feed entry or
operator row that answers the id without stating `image_input` leaves the
flag unknown, even when the models.dev row below it says text-only, and
images are sent. To refuse images on an id such a row answers, state
`image_input` on that row, within the rollout limits below
(`TestResolveCatalog_TheAnsweringRungAnswersTheImageFlag`). A native route
reads the models.dev row of the provider that serves it, live then
snapshot. A GLM id on the anthropic wire (`anthropic/glm-*` or a bare
`glm-*`) reads z.ai's rows (`zai-coding-plan`, then `zai`). `moonshot`
reads `moonshotai`, `vertex` reads `google-vertex` and `bedrock` reads
`amazon-bedrock`. `foundry` ids are deployment names, so they state
nothing. A Claude model on an Anthropic route always takes images. The key
is always provider-qualified: a bare id's publishers disagree (glm-5.3 is
text-only at some providers and takes images at others), and that
disagreement stays unknown, which sends images. Only a stated `false`
refuses. Deployments of one LiteLLM `model_name` fold to false if any says
false, and to true only if all say true. PLIAGE's `/model/info` states
`supports_vision` for no model. The scaleway row answers only for an id no
feed entry and no operator row answers: there, `scaleway/glm-5.3` is
text-only, and the vision models above take images. `iterion models` prints
the flag in its `IMAGES` column, and the capability API returns it as
`image_input`.

Mixed versions during a rollout: a binary older than `image_input` parses
the operator table and feed entries strictly, so it refuses a table that
states it.

- An older runner refuses the whole of `OPENAI_COMPATIBLE_MODELS`, and
  every gateway id it carries resolves to unknown. Write `image_input` into
  the operator table only once every runner reading it runs this release.
- An older server replica refuses the write of a declared feed stating it
  and folds nothing from that feed, prices included. An older replica
  holding the reconciler's lease fails to refresh an exec feed that prints
  it. That failing refresh writes the entries back with their prices and
  without `image_input`, so images are sent to those ids until this release
  refreshes the feed. Write `image_input` into a declared or exec feed only
  once no older server replica can fold or refresh it
  (`TestAnOlderBinaryRefusesATableStatingImageInput`,
  `TestAnOlderReplicasFailingRefreshDropsAnExecFeedsImageInput`).

On a refusing route, an image tool returns a recoverable tool error
(`tool.ImagesRefusedError`). It names the route, keeps the tool's own
description of the image, and says calling again will not help, so get
the content another way. A tool image replayed from
another route's history (a fallback) goes out as a marker. A prompt image
refuses the request before it is sent (`RequestImagesRefusedError`).
`ITERION_IMAGE_ACCEPTANCE` is operator-only and crosses into the sandbox
with the bounds. `auto` (the default) follows the catalog, `send`
overrides a stated refusal, and `refuse` refuses images on every route. Any
other value fails the generation. `claude_code` serves Anthropic models
(all of them take images) through its own CLI, which bounds its own images:
none of this applies there.
