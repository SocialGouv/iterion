---
name: synthesis-scoring
description: Editorial rubric for the feed-watch digest agent — how to group, rank, semantically dedup and write a chat digest that respects the category's editorial brief.
---

# Digest synthesis rubric

You are composing ONE chat message (Mattermost/Slack markdown) from a
queue of feed items. The editorial brief in `input.editorial` is the
authority on audience, language and priorities — this rubric is the
method.

## 1. Group before you rank

Multiple feeds routinely carry the SAME story (vendor blog + news site
+ aggregator). One story = one entry: pick the best primary link
(original source > secondary coverage), fold the other links in as
`([also](url2))`. Grouped items each count in `items_included`.

## 2. Semantic dedup against previous digests

`input.recent_topics` contains the last sent digests. An item already
covered there is dropped (list it in `items_skipped_duplicates`)
UNLESS there is a material update — new patch, active exploitation,
major follow-up — in which case include it explicitly marked as a
follow-up. Never re-announce the same release twice.

## 3. Ground the top items

`web_fetch` at most ~6 URLs — the items whose takeaway will lead the
digest. The article content beats the RSS excerpt: extract the ONE
fact that matters (severity, version, date, number). A failed fetch is
not an error; keep the feed summary. Never fetch every item.

## 4. Ranking

Order by impact for the audience described in the editorial brief:

1. act-now items (actively exploited vulns, urgent advisories,
   breaking changes requiring action) — visually marked (🔴/⚠️);
2. significant news (major releases, security patches, ecosystem
   shifts);
3. worth-knowing (tools, articles, analyses);
4. everything else as ONE compact "also seen" line of linked titles —
   no takeaways.

## 5. Writing the message

- One short headline line first (also emitted as `headline`).
- Entries: `**[title](url)** — takeaway in 1–2 sentences`, in the
  brief's language. The takeaway is the "so what", not a paraphrase of
  the title.
- Sections with a small emoji header when there are natural groups;
  no deep nesting — chat renders flat.
- ≤ ~10000 characters. If `input.overflow_count > 0`, end with an
  explicit "N older queued items were dropped unprocessed" line.
- The message is the digest — no meta ("here is your digest"), no
  self-reference, no invented items or facts. Every entry maps to
  input items.
