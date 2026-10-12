"""Exit 0 when AFTER differs from BEFORE only in tap values, 1 otherwise.

Usage: python3 -I tap_values_only.py BEFORE AFTER

The two files are a Homebrew tap file before and after a pull request or a
merge-queue entry that skipped tests.yml's heavy checks as a tap update
(scripts/ci/tap-entry-check.sh). A tap update is what
scripts/update-brew-tap.sh writes: it moves `version` and `sha256` values,
each from one value it writes — a dotted numeric version, a 64-hex digest —
to another, and touches nothing else.

So the files are compared as BYTES, line for line: the same number of lines,
and every line that differs is a `version` or `sha256` line whose key,
indentation, spacing and line ending are identical on both sides and whose
two values are canonical. Bytes, because text mode translates line endings:
a CR or CRLF rewrite would compare equal, and brew reads it differently.
"""

import re
import sys

LINE = re.compile(rb'^([ \t]*)(version|sha256)([ \t]+)("[^"]*")([ \t\r]*)$')
VALUE = {
    b"version": re.compile(rb'"[0-9]+(?:\.[0-9]+)*"'),
    b"sha256": re.compile(rb'"[0-9a-f]{64}"'),
}


def lines(path):
    with open(path, "rb") as f:
        return f.read().split(b"\n")


def values_only(before, after):
    if len(before) != len(after):
        return False
    for a, b in zip(before, after):
        if a == b:
            continue
        ma, mb = LINE.match(a), LINE.match(b)
        if not (ma and mb) or ma.group(1, 2, 3, 5) != mb.group(1, 2, 3, 5):
            return False
        value = VALUE[ma.group(2)]
        if not (value.fullmatch(ma.group(4)) and value.fullmatch(mb.group(4))):
            return False
    return True


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: tap_values_only.py BEFORE AFTER")
    sys.exit(0 if values_only(lines(sys.argv[1]), lines(sys.argv[2])) else 1)
