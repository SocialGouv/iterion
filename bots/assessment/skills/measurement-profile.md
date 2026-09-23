---
name: measurement-profile
description: The default measurement profile — metrics, canonical exclusions, domain of applicability, synthetic anchor and size thresholds, versioned together. Read to understand what the published size letter means and what it does not.
---

# The measurement profile

A size letter is a **ratio to an anchor**, cut into bands. It is therefore not
a property of a repository: it is a property of a repository *read through a
scale*. The scale is this file, and the assessment publishes the letter with
this profile's identifier and version beside it, always.

That is a convention, stated as one rather than hidden. Two repositories sized
under different profiles are not comparable. A letter quoted without its
profile is a false quotation.

## What the profile fixes, and why all five together

| part | what it decides |
|---|---|
| **metrics** | which four quantities describe amplitude |
| **canonical exclusions** | what never counts as first-party, whatever a survey declares |
| **domain** | the repositories the letter means anything for |
| **anchor** | the reference the ratios are taken against |
| **thresholds** | where one band ends and the next begins |

They are versioned as ONE object because changing any of them changes the
letter. A profile that let the anchor move while the version stayed put would
publish two incomparable letters under one name — the precise failure the
version number exists to prevent.

## The index

The index is the **geometric mean of the four ratios** to the anchor. The
geometric mean is deliberate: it makes the index scale-free and it makes no
single axis dominate. It also has a consequence that the domain exists to
handle — **one null axis drives the whole index to zero**, so a repository
with no entrypoints at all (a library, a batch job, a data pipeline) would be
sized `XS` by construction, regardless of how large it is.

That is not a defect to be smoothed away with a floor value. It is the scale
saying it does not apply.

## The domain, and what happens outside it

The letter is published only when the repository is **inside the domain**:
every metric strictly positive, and a first-party source size above the floor
below. Outside it, the assessment publishes the **raw measurements** and the
size `not-applicable`, naming the axis that put it outside.

A `not-applicable` is a real answer. It is not a failure of the measurement
and it must not be read as "small".

## Uncertainty is published with the letter

Two of the four metrics are small discrete counts, where one declaration
either way is a plausible disagreement between two careful surveys. The
assessment therefore recomputes the index with ±1 applied to each discrete
count, on the project side and on the anchor side, and publishes the **set of
bands** those variants land in. When that set has more than one member, the
repository sits on a boundary and the letter should be read as the range, not
as the point.

## The anchor is synthetic

The anchor below is a **convention**, not a measured project. Its four values
are round numbers chosen to put a mid-sized service near the middle of the
scale. This matters twice: nothing about anybody's repository is disclosed by
publishing it, and nobody should read the anchor as a claim that a real
project of that shape exists or is typical.

<!-- iterion:profile
{
  "id": "public-default",
  "version": "1.0.0",
  "metrics": [
    {"key": "first_party_lines", "label": "first-party lines of source (tests, data and vendored code excluded)", "discrete": false},
    {"key": "entrypoints", "label": "declared entrypoints", "discrete": false},
    {"key": "deployables", "label": "deployed artefacts", "discrete": true},
    {"key": "systems", "label": "distinct systems the application talks to", "discrete": true}
  ],
  "canonical_exclusions": [
    {"kind": "vendored", "why": "third-party source carried in the tree is not written here"},
    {"kind": "generated", "why": "a file declaring itself generated in its own header is not first-party"},
    {"kind": "locked", "why": "a lock file is a resolved graph, not source"},
    {"kind": "data", "why": "fixtures and datasets are inputs, not source"},
    {"kind": "tests", "why": "counted as tests, never as source"}
  ],
  "domain": {
    "all_metrics_positive": true,
    "min_first_party_lines": 2000,
    "statement": "a deployed application with at least one entrypoint, one deployable and one distinct system, above the first-party floor. A library, a batch with no entrypoint, or a repository below the floor is OUTSIDE the domain and gets raw measurements with no letter."
  },
  "anchor": {
    "synthetic": true,
    "first_party_lines": 20000,
    "entrypoints": 40,
    "deployables": 2,
    "systems": 4
  },
  "bands": ["XS", "S", "M", "L", "XL", "XXL"],
  "thresholds": [0.5, 1.0, 2.0, 4.0, 8.0]
}
-->

## Using another profile

An operator may point the bot at their own profile with `--var
profile_path=<path in the workspace>`, carrying the same block. The published
letter then names THAT profile and version. The default exists so that the
common case is a scale somebody reasoned about rather than one improvised per
run — and so that the failure mode of a hastily written profile is visible in
the document, under a name that is not `public-default`.
