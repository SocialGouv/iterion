---
name: upgrade-archetypes
description: The behaviour-drift classes every MAJOR crossing ships — a stack-agnostic sweep grammar. Instantiate each class from the migration notes of the exact major being crossed, run the derived probes mechanically, and leave the record in the tree.
---

# What a major breaks while everything stays green

A major version is a list of behaviour changes someone else already wrote down.
The failures it ships are therefore not surprises — they are documented classes,
instantiated in whichever idiom the target uses. A lot that crosses a major and
does not sweep for them is betting that a codebase written under the old
semantics happens to use none of the changed ones. Measured across real
migrations, that bet loses several times per major, always in the same shape:
**code identical to the baseline, semantics changed underneath it, every build
and every read-only page still green.**

The sweep below is a grammar, not a checklist to copy: the bot knows no
framework, so each class names a KIND of drift, what stays green while it
ships, and how to derive the concrete probe for the stack at hand. The
derivation source is always the same — **the migration notes of the exact
major the lot crosses** — never memory of "how that framework usually behaves".

<!-- iterion:upgrade-archetypes
[
  {"class":"routing_semantics","crossing":"web framework",
   "ships_green_because":"only redirects and URL edge forms change; every route the corpus hits by its canonical spelling still answers",
   "blind_spot":"redirect targets and link forms written under the old matcher — trailing slashes, case folding, suffix matching, encoded characters",
   "derive":"from the notes, list every URL-matching behaviour that changed; grep the tree for redirect targets and generated links using the OLD form; probe each hit against the running app"},
  {"class":"binding_conversion","crossing":"web framework",
   "ships_green_because":"forms that only READ render fine; the break lives in the submit path, often only on its error-replay branch",
   "blind_spot":"type conversion the old framework registered implicitly — entity ids in hidden fields, string-to-enum, string-to-list — now unregistered or stricter",
   "derive":"from the notes, list removed or changed converter/formatter registrations; inventory every non-scalar field bound from a form (hidden fields included); submit each form's error path and its corrected retry"},
  {"class":"query_strictness","crossing":"ORM or query layer",
   "ships_green_because":"the query only runs when a specific filter or branch is exercised; unfiltered listings never build the broken predicate",
   "blind_spot":"comparisons the old layer tolerated and the new one rejects — scalar vs collection, enum vs serialisable, implicit type coercion in criteria",
   "derive":"from the notes, list the strictified constructs; grep every dynamically-built query (criteria builders, specifications) for them; execute each filter of each search screen once with a real value"},
  {"class":"template_attr_semantics","crossing":"front framework or template engine",
   "ships_green_because":"the DOM still contains the element; only an attribute's rendered FORM changed, and no functional test reads attribute spelling — and an HTTP capture sees the TEMPLATE, never the client-rendered DOM",
   "blind_spot":"boolean and enumerated attributes rendered differently (present-with-false vs absent), styling hooks that key on attribute presence — ESPECIALLY on elements where the attribute has no native meaning (disabled on an anchor): the framework's rendering choice is the only semantics there, and a false that renders as present greys a control that still works",
   "derive":"from the notes, list attribute-rendering changes; then grep EVERY template binding of a boolean-ish attribute (disabled, checked, selected, readonly, required, hidden, open) on ANY element kind, form or not, anchors included — a form-bindings inventory misses exactly the anchors; for each hit, drive the state BOTH ways in a rendered DOM (browser or DOM engine, never the template text) and inspect the attribute AND the computed style of the control",
   "note":"measured twice on one real crossing: the sweep instantiated this class for attribute-NAME folding and still shipped a present-with-false disabled on an anchor, greyed by the CSS framework — because it inventoried form bindings only and probed templates, not the rendered DOM"},
  {"class":"dialect_functions","crossing":"database engine",
   "ships_green_because":"the function only appears in a filter or report branch; every query the seeds exercise uses portable constructs",
   "blind_spot":"functions, operators and implicit casts that exist on one engine only, buried in native queries and criteria builders",
   "derive":"list every raw function name used in queries; check each against the target engine's own function catalogue; execute the code path that builds each one"},
  {"class":"seed_allocator_state","crossing":"database engine",
   "ships_green_because":"seeded reads are perfect; the allocator is only consulted on the first INSERT the application makes",
   "blind_spot":"id allocators (sequences, identity columns) left behind data seeded at explicit ids — the first creation dies on a duplicate key",
   "derive":"for every seeded table, verify the allocator's next value exceeds the seeded maximum, ON the target engine; then create one row through the application",
  "note":"the fixture rule and its proof live in the net's own doctrine — see the golden-master bot's deterministic-fixture skill; this class exists so an ENGINE-CROSSING lot re-runs that proof after conversion"},
  {"class":"collation_semantics","crossing":"database engine or locale",
   "ships_green_because":"every seeded lookup uses the exact stored case, and listings are compared unordered or sorted by numeric id",
   "blind_spot":"case folding in lookups (logins, searches) and TEXT ordering — both flip silently between collations",
   "derive":"compare the old and new collation's folding and ordering rules; probe one lookup with a re-cased value and one listing ordered on a textual key, on BOTH engines when two are kept",
   "note":"the net's case_pair, text_sort and auth_case corpus probes are the permanent form of this sweep; an engine-crossing lot must see them green on the NEW engine, not only on the old one"},
  {"class":"silent_default_flips","crossing":"any major",
   "ships_green_because":"a default that flips changes behaviour with zero diff in the tree — there is nothing to review",
   "blind_spot":"security postures, encodings, strictness flags and feature toggles whose default changed between majors",
   "derive":"from the notes, extract the table of changed defaults; for each one the tree does not explicitly set, decide KEEP (set the old value explicitly) or ADOPT (record it as an intended behaviour change in the lot report) — deciding by silence is the only wrong answer"}
]
-->

## How the sweep is run — mechanically, and on the record

For a lot whose contract declares `crosses_major: true`:

1. **Derive.** Read the migration notes of the major actually crossed. For each
   class above, write the concrete probes for THIS stack: grep patterns over
   the tree, and runtime probes against the running application. A class with
   no instantiation in this stack is written down as such, with one line of
   why.
2. **Run.** Execute every derived probe. A grep hit is not a finding — it is a
   site to probe at runtime. A runtime divergence from the baseline's behaviour
   is a finding.
3. **Record.** Write the sweep record into the tree (the plan contract says
   where) — one section per class: derived probes, hits, findings, and what
   was done with each. The lot's exit_gate checks the record EXISTS; its
   content is judged by the reviewer and, above all, by the net.
4. **Close the loop with the net, not with the sweep.** A finding on a surface
   the corpus does not watch is TWO problems: the regression, and the corpus
   hole that hid it. Fix the first in the lot; for the second, file the
   net-extension request through the ledger, as always — the sweep never
   touches the oracle. The sweep directs attention; only the net proves.

The sweep is bounded by the notes plus the eight classes — it is not an
invitation to audit the world. Its whole cost is one reading of a document the
upgrade required reading anyway, plus a handful of greps and probes; its yield,
measured on a real audit, was every single post-migration defect found.
