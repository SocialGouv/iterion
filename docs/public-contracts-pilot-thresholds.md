# Frozen representative pilot thresholds for #1165

These are isolated, deterministic slices of three existing workflows. They do
not run the projects, invoke models, create media, or modify project files.
The source paths identify the behavior chosen for the slices; their SHA-256
values prevent silently switching the reference after measurement.

| Pilot | Reference workflow and SHA-256 | Frozen slice |
| --- | --- | --- |
| Shorts | `video/shorts/bots/shorts-episode/scene-unit.bot`, `a7d1a5695c542d2cecd67cb88b22dfde1dff931e7ec5a59ffd87a856232e2913` | `prepare_keyframe_stage` → keyframe jobs → `publish_keyframes` |
| Town | `game/town/bots/town-vertical-pipeline/bots/planner/subbots/epic-acceptance-review.bot`, `f8c37ed234f2d8fea224518d21b46731c8c636c8e25261bd828fa1df4d1a60c9` | `prepare_epic_images` → per-view image jobs → `seal_epic_images` |
| Tabarria | `video/Tabarria/bots/bestof-lab/animal-range.bot`, `887298f94de5f28752685b6ee6b50836acccf4065057371f6b72af446a68de25` | `prepare_stills` → per-still jobs → `collect_stills` |

Each slice gets a committed legacy control-flow fixture and a native public
contract/graph fixture. A declared fake executor returns deterministic values
and fake effect handlers record calls. No hidden behavior of the source bot is
certified by the fixture: human reviews, loops, provider costs, subbot setup,
media quality and project-specific persistence are outside these slices.

For 0, 1 and 4 jobs, both fixtures must produce the same ordered list of
deliverable identifiers and the same final summary value. Every identifier
must be unique, nonempty and attributable to its input item. The expected
invocations are one preparation, exactly N item executions, and one collection
for each fixture; a zero-item native map performs no item execution. Each
required public product must be present before native success. At most two
item jobs may execute together. With four jobs and the fixture's controlled
overlap, at least two must overlap in both implementations. Retries and fake
paid-effect calls must be zero in the success cases.

Measure three isolated runs per input size after the fixtures and thresholds
are committed. Record median wall time, invocation counts, peak item
concurrency, retries and deliverables for each implementation. The native
median must not exceed `max(2 × legacy median, legacy median + 100 ms)`; this
wide bound detects gross regression without treating scheduler noise as a
product promise. Tokens and monetary cost are **unknown** in a fake executor
and must be reported that way. A failure requires code repair or a new
documented threshold version committed before fresh measurements. The report
must name this document's exact commit hash and make no claim about full
workflow performance from these slices.
