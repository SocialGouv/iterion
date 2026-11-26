# Naming a catalog bot — a colour, or ends like iterion

Read it when you name a new catalog bot, rename one, or touch a manifest's
`display_name`.

A catalog bot has three names:
- its **slug** (`adr-rechallenge`): the folder, the routes, the keys;
- its **persona** (`display_name: White Elephant`): what people say;
- its **slash command** (`/white-elephant`): what people type.

This page governs the persona and the command it implies. The slug stays
functional and never follows the persona.

## The rule

> **An iterion bot carries a colour, or ends like iterion.**

A persona carries at least one **mark**:

- **Palette**: two words, exactly one of them a colour from the closed
  lexicon below. Examples: `Code Red`, `White Elephant`, `Grey Area`,
  `Green Room`, `Deep Blue`.
- **`-ion`**: the last word ends in `-ion`, the way iterion does, in one word
  or in two. Example: `Right Direction`.

A persona may carry both marks.

The two marks keep the filter honest. Measured against 143 dev-tool and
agent names, the old `-y`/`-i` habit lets 13 % through (Cody, Trivy, Sentry,
Husky…). A colour or an `-ion` ending lets 5 % through.

**Colour lexicon:** red, orange, yellow, green, blue, purple, violet, pink,
black, white, grey, gray, brown, gold, golden, silver. The lexicon is English
only. Adding a colour is a change to this page and to the test, never a
manifest-only edit.

## What review judges, in this order

1. **It makes people smile and it sticks.** A known phrase that puns on the
   role beats an invented word: `White Elephant` re-challenges the decision
   that became a burden.
2. **It says what the bot does.** Between two funny names, the more explicit
   one wins.
3. **The reference holds when you unpack it.** A guard is not named after a
   hero who loses his duel.

Then two checks that need a search engine, not a test:
- **Trademark and homonym.** Search the name in the software, AI and dev
  field before adopting it. DevBoxer, One Direction and Blue Sky were
  dropped this way: an agent platform already uses the first, a defended
  trademark covers the second, and the third sits next to Bluesky.
- **Connotation.** A colour carries meaning: Blue Whale (the self-harm
  "challenge"), Amber Alert, Red Cross (a protected emblem) and Red Pill
  are out.

## What the test enforces

[`bots/catalog_naming_test.go`](../../../bots/catalog_naming_test.go) walks
every catalog manifest, but not `examples/`, and fails when a persona breaks
one of these rules:

- **Mark.** The persona carries a mark, or it is a joker or grandfathered.
- **Shape and bounds.** Only ASCII letters and digits, the first word
  starting with a letter, words separated by one space. This keeps the
  kebab-case form a valid command. At most two
  words. At most 16 characters. The last word is at most 10 letters.
- **Distinctness.**
  - A new persona shares no word with another bot's slug or persona.
    A colour counts once whatever its spelling (grey and gray, gold and
    golden): there is one "Red" bot, one grey one, and one gold one.
  - No two personas sit within edit distance 2 of each other, except the pairs
    listed as grandfathered.
- **Command.** A new persona or a joker whose bot declares a slash command
  answers to its kebab-case form. That form is either the command's name or
  an alias, compared the way routing compares, trimmed and lower-cased.
- **Ratchets.**
  - An entry that no longer matches a bot fails the test, in each of the
    three lists: jokers, grandfathered personas, grandfathered near pairs.
  - The two grandfathered lists only shrink: a count pins their size, and
    a removal lowers it.

Grandfathered personas are exempt from the shape, bounds, shared-word and
command rules. They still count when a new name is checked against them.

**Jokers** are named exceptions for a name too good to lose. Each one is
added in review, with its reason written next to it. `Devboxing` is one: it
says "devbox" outright and puns on boxing.

**Grandfathered** names are the personas that predate the rule (Revi, Vigie,
Obsy, Billy…). They stay. Renaming one is a case-by-case decision, and its old
command stays as an alias.

## Renames

| slug | before | after | mark |
|---|---|---|---|
| supply-shield-cve | Vulny | Code Red | palette |
| issue-triage | Triagy | Right Direction | -ion |
| adr-rechallenge | ReArchi | White Elephant | palette |
| arbitrate | Themis | Grey Area | palette |
| review-env | Envy | Green Room | palette |
| devbox-setup | Devy | Devboxing | joker |
| evolve | Evoly | Deep Blue | palette |

**Renaming a bot**:
- Change `display_name` and `icon` in its manifest.
- Add the kebab-case command and keep the old one in `aliases`.
- On each repository integration that enables the bot, take the bot out
  of the integration's bot set and put it back. A provisioned webhook keeps
  the command map it was built with. A re-provision with an unchanged bot
  set leaves that map as it is, so the new command routes nowhere on that
  repository until the bot set changes. The old command keeps working.
- Update the matching emoji in `studio/src/lib/personas.ts`.
- Update the living text that cites the old name (prompts, skills, README,
  docs). Dated run bilans and ADRs keep the name they were written with.
  The bot's README and bilan headings say `(formerly X)`.
- Regenerate the catalog (`iterion bots regen-catalog`), the maps
  (`task map:gen`) and the reading golden
  (`task corpus:reading -- --write`).

**Never renamed with the persona:** the slug, a `gate_context` (repositories'
rulesets require it by name) and skill file names.
