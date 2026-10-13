# Credits

Acci (`rgaa-audit`) is an orchestration. Every accessibility judgement a run
makes — the static rules, the worklist of criteria a static pass cannot
decide, the verdict contract the fold enforces, the French report, its
integrity checks and the issue set — belongs to the engine below. The bundle
carries no normative text: the engine writes one brief per criterion at run
time, and the agents read those.

- **ultra11y** — [SocialGouv/ultra11y](https://github.com/SocialGouv/ultra11y),
  MIT licence, by Maxime Garcia (@maxgfr) and Victor Zeinstra (@Viczei).
  Thank you for the engine, for the orchestrator contract written with this
  bot in mind (`skills/ultra11y/references/orchestrators.md`), and for a CLI
  whose refusals are named. The version this bundle pins, with the sha256 of
  its entry file, is in `ultra11y.pin`; the dependency closure is in
  `scripts/ultra11y-engine/package-lock.json` at the repository root.
- **RGAA 4.1.2** — Référentiel général d'amélioration de l'accessibilité,
  DINUM, published under the Licence Ouverte 2.0; carried by the engine's
  `rgaa` pack, rendered into the per-criterion briefs and the French report.
- **WCAG 2.2** — Web Content Accessibility Guidelines, W3C, under the W3C
  Document License; carried by the engine's core audit.
- **etalab-ia/skills** (MIT) — the source of the RGAA paraphrases earlier
  versions of this bot shipped as `rgaa-criteria-*` skills. Those copies
  are retired: paraphrased normative text drifts, and the engine renders
  the standard's own tests instead.

The shared `iterion-board` skill is iterion's own. Everything else in this
bundle is MIT, like the rest of iterion.
