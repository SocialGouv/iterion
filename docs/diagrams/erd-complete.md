# The complete ERD — every collection, one diagram

The whole Mongo data model in a single entity-relationship diagram: all 80 collections plus the four marked non-Mongo participants, and every relationship the per-page diagrams draw — consolidated, no attributes. The per-entity detail (keys, value sets, indexes, twins) lives in the six data-model pages; this page answers "what relates to what" at a glance, and its Mermaid source, `erd-complete.mmd` next to this page, is kept in step with them by hand.

The rendered SVG lives next to this page: [erd-complete.svg](erd-complete.svg).
Open it in a browser tab of its own for the full size — it is a vector image,
so it stays crisp at any zoom and the page scrolls natively.

**The image lags its source.** The committed SVG was rendered before the
session changes of #1668 and lacks what `erd-complete.mmd` now holds: the
`session_revocations` collection and its two relationships (to `users` and
to `sessions`), the two self-relationships of `sessions` (`rotated_from`,
`lineage`), and four relationships the run pages draw — `assistant_missions`
and `assistant_run_watches` to `runs` by `assistant_run_id`, and `runs` to
`runs` by `forked_from` and by `delegation.source_run_id`. Read those from
the source or the data-model pages until the image is rendered again.

![The complete ERD as last rendered — it predates the relationships listed above](erd-complete.svg)

*Re-render the SVG after editing the source with
`mermaid-cli -i erd-complete.mmd -o erd-complete.svg`. The theme and fonts
the committed image was rendered with are not in the tree, so a render
changes its look as well as its content.*


*Drawn from the code at commit `61a305262` and held to the same source as the per-page diagrams. The [overview, conventions and glossary](README.md) explain the notation.*
