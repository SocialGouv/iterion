#!/usr/bin/env python3
"""Convert a GitBook-flavoured markdown tree into an MkDocs (Material) source.

Input : a GitBook space dir (README.md + SUMMARY.md + pages, {% hint %} /
        {% stepper %} / {% tabs %} blocks).
Output: <out>/mkdocs.yml + <out>/docs/** ready for `mkdocs build`.

Fail-closed: any {% ... %} construct this script does not know how to render
aborts the conversion with the offending file:line, rather than silently
shipping a page with raw template noise in it.
"""

import argparse
import os
import re
import shutil
import sys

HINT_OPEN_RE = re.compile(r'^\s*\{%\s*hint(?:\s+style="([a-z]+)")?\s*%\}\s*$')
HINT_CLOSE_RE = re.compile(r'^\s*\{%\s*endhint\s*%\}\s*$')
STEPPER_OPEN_RE = re.compile(r'^\s*\{%\s*stepper\s*%\}\s*$')
STEPPER_CLOSE_RE = re.compile(r'^\s*\{%\s*endstepper\s*%\}\s*$')
STEP_OPEN_RE = re.compile(r'^\s*\{%\s*step\s*%\}\s*$')
STEP_CLOSE_RE = re.compile(r'^\s*\{%\s*endstep\s*%\}\s*$')
TABS_OPEN_RE = re.compile(r'^\s*\{%\s*tabs\s*%\}\s*$')
TABS_CLOSE_RE = re.compile(r'^\s*\{%\s*endtabs\s*%\}\s*$')
TAB_OPEN_RE = re.compile(r'^\s*\{%\s*tab\s+title="([^"]*)"\s*%\}\s*$')
TAB_CLOSE_RE = re.compile(r'^\s*\{%\s*endtab\s*%\}\s*$')
EMBED_RE = re.compile(r'^\s*\{%\s*embed\s+url="([^"]+)"\s*%\}\s*$')
CONTENT_REF_OPEN_RE = re.compile(r'^\s*\{%\s*content-ref\s+url="([^"]+)"\s*%\}\s*$')
CONTENT_REF_CLOSE_RE = re.compile(r'^\s*\{%\s*endcontent-ref\s*%\}\s*$')
ANY_TAG_RE = re.compile(r'\{%.*?%\}')
FENCE_RE = re.compile(r'^\s{0,3}(```|~~~)')
HEADING_RE = re.compile(r'^\s*#{1,6}\s+(.*\S)\s*$')

# GitBook hint styles → Material admonition types (all four exist natively),
# with French display titles (the corpus is end-user documentation in French).
HINT_STYLES = {"info": "info", "warning": "warning", "danger": "danger", "success": "success"}
HINT_TITLES = {"info": "Info", "warning": "Attention", "danger": "Danger", "success": "Bon à savoir", "note": "Note"}


class ConvertError(Exception):
    pass


def convert_page(text, relpath):
    out = []
    in_fence = False
    fence_marker = None
    # Stack of ('hint'|'step'|'tab', indent) — content inside is re-indented.
    stack = []
    # Step numbering restarts at each {% stepper %}.
    step_no = 0
    pending_step_title = False  # swallow the first heading inside a step as its title

    def indent():
        return "    " * len(stack)

    for lineno, line in enumerate(text.splitlines(), 1):
        fence = FENCE_RE.match(line)
        if fence:
            if not in_fence:
                in_fence, fence_marker = True, fence.group(1)
            elif fence.group(1) == fence_marker:
                in_fence, fence_marker = False, None
            out.append(indent() + line if stack else line)
            continue
        if in_fence:
            out.append(indent() + line if stack else line)
            continue

        m = HINT_OPEN_RE.match(line)
        if m:
            style = HINT_STYLES.get(m.group(1) or "info", "note")
            out.append(indent() + '!!! %s "%s"' % (style, HINT_TITLES[style]))
            stack.append(("hint", indent()))
            continue
        if HINT_CLOSE_RE.match(line):
            _pop(stack, "hint", relpath, lineno)
            continue

        if STEPPER_OPEN_RE.match(line):
            step_no = 0
            continue  # pure container — steps carry the structure
        if STEPPER_CLOSE_RE.match(line):
            continue
        m = STEP_OPEN_RE.match(line)
        if m:
            step_no += 1
            # Title is filled in when the step's first heading is seen.
            out.append(indent() + '!!! abstract "Étape %d"' % step_no)
            stack.append(("step", indent()))
            pending_step_title = True
            continue
        if STEP_CLOSE_RE.match(line):
            _pop(stack, "step", relpath, lineno)
            pending_step_title = False
            continue

        if TABS_OPEN_RE.match(line):
            continue  # container — tabs are siblings at the same indent
        if TABS_CLOSE_RE.match(line):
            continue
        m = TAB_OPEN_RE.match(line)
        if m:
            out.append(indent() + '=== "%s"' % m.group(1).replace('"', "'"))
            stack.append(("tab", indent()))
            continue
        if TAB_CLOSE_RE.match(line):
            _pop(stack, "tab", relpath, lineno)
            continue

        m = EMBED_RE.match(line)
        if m:
            out.append(indent() + "<%s>" % m.group(1))
            continue
        m = CONTENT_REF_OPEN_RE.match(line)
        if m:
            # The inner line repeats the link; the closing tag ends the block.
            out.append(indent() + "→ [%s](%s)" % (m.group(1), m.group(1)))
            stack.append(("content-ref", indent()))
            continue
        if CONTENT_REF_CLOSE_RE.match(line):
            _pop(stack, "content-ref", relpath, lineno)
            continue

        if ANY_TAG_RE.search(line):
            raise ConvertError("%s:%d: unhandled GitBook construct: %s" % (relpath, lineno, line.strip()))

        if pending_step_title:
            h = HEADING_RE.match(line)
            if h:
                # Promote the step's own heading into the admonition title.
                out[-1] = out[-1][: out[-1].rindex('"Étape')] + '"Étape %d — %s"' % (step_no, h.group(1).replace('"', "'"))
                pending_step_title = False
                continue
            if line.strip():
                pending_step_title = False

        if stack and stack[-1][0] == "content-ref":
            continue  # inner repetition of the link — already rendered

        out.append(indent() + line if (stack and line.strip()) else (line if not stack else ""))

    if stack:
        raise ConvertError("%s: unclosed %s block at EOF" % (relpath, stack[-1][0]))
    return "\n".join(out) + "\n"


def _pop(stack, kind, relpath, lineno):
    if not stack or stack[-1][0] != kind:
        raise ConvertError("%s:%d: end%s without matching open" % (relpath, lineno, kind))
    stack.pop()


SUMMARY_SECTION_RE = re.compile(r'^##\s+(.*\S)\s*$')
SUMMARY_ITEM_RE = re.compile(r'^(\s*)[*-]\s+\[([^\]]+)\]\(([^)]+)\)\s*$')


def build_nav(summary_text, relpath="SUMMARY.md"):
    """Parse a GitBook SUMMARY.md into an MkDocs nav structure (list of dicts)."""
    nav = []
    section_items = None  # items list of the current '## Section'
    parents = []  # stack of (indent_len, children_list) for nested items

    def target(items, indent_len):
        while parents and parents[-1][0] >= indent_len:
            parents.pop()
        return parents[-1][1] if parents else items

    for lineno, line in enumerate(summary_text.splitlines(), 1):
        if not line.strip() or line.startswith("# "):
            continue
        m = SUMMARY_SECTION_RE.match(line)
        if m:
            section_items = []
            nav.append({m.group(1): section_items})
            parents = []
            continue
        m = SUMMARY_ITEM_RE.match(line)
        if m:
            indent_len, title, path = len(m.group(1)), m.group(2), m.group(3)
            entry = {title: path}
            items = section_items if section_items is not None else nav
            dest = target(items, indent_len)
            dest.append(entry)
            children = []
            parents.append((indent_len, children))
            entry["_children"] = children
            continue
        raise ConvertError("%s:%d: unrecognised SUMMARY line: %s" % (relpath, lineno, line.strip()))

    def finalize(entries):
        result = []
        for e in entries:
            (title, path) = next((k, v) for k, v in e.items() if k != "_children")
            kids = finalize(e.get("_children", []))
            if kids:
                result.append({title: [path] + kids})
            else:
                result.append({title: path})
        return result

    out = []
    for e in nav:
        if "_children" in e:
            out.extend(finalize([e]))
        else:
            (title, items) = next(iter(e.items()))
            out.append({title: finalize(items)})
    return out


def dump_yaml(value, level=0):
    """Minimal YAML emitter for the nav/config structures this script builds."""
    pad = "  " * level
    lines = []
    if isinstance(value, list):
        for item in value:
            if isinstance(item, (dict, list)):
                first, rest = _dump_inline(item, level + 1)
                lines.append(pad + "- " + first)
                lines.extend(rest)
            else:
                lines.append(pad + "- " + _scalar(item))
    elif isinstance(value, dict):
        for k, v in value.items():
            if isinstance(v, (dict, list)) and v:
                lines.append(pad + _scalar(k) + ":")
                lines.extend(dump_yaml(v, level + 1))
            else:
                lines.append(pad + _scalar(k) + ": " + _scalar(v))
    return lines


def _dump_inline(item, level):
    if isinstance(item, dict) and len(item) == 1:
        (k, v) = next(iter(item.items()))
        if isinstance(v, (dict, list)):
            rest = dump_yaml(v, level + 1)
            return _scalar(k) + ":", rest
        return _scalar(k) + ": " + _scalar(v), []
    sub = dump_yaml(item, level)
    return sub[0].strip(), sub[1:]


class Raw(str):
    """A scalar emitted verbatim (YAML tags like !!python/object/apply)."""


def _scalar(v):
    if isinstance(v, Raw):
        return str(v)
    if isinstance(v, bool):
        return "true" if v else "false"
    s = str(v)
    if re.search(r'[:#\[\]{}&*!|>%@`"\']', s) or s != s.strip():
        return '"' + s.replace('"', '\\"') + '"'
    return s


MD_LINK_RE = re.compile(r'(\[[^\]]*\]\()([^)#\s]+\.md)((?:#[^)]*)?\))')


def fix_root_relative_links(docs_dir):
    """Rewrite page links that only resolve from the space ROOT (a GitBook
    corpus habit; broken on GitBook too) into correct page-relative links.
    Returns the number of rewritten links."""
    fixed = 0
    for root, _dirs, files in os.walk(docs_dir):
        for name in files:
            if not name.endswith(".md"):
                continue
            path = os.path.join(root, name)
            with open(path, encoding="utf-8") as f:
                text = f.read()

            def repl(m):
                nonlocal fixed
                target = m.group(2)
                if target.startswith(("http://", "https://", "mailto:")):
                    return m.group(0)
                if os.path.isfile(os.path.normpath(os.path.join(root, target))):
                    return m.group(0)
                from_root = os.path.normpath(os.path.join(docs_dir, target))
                if os.path.isfile(from_root):
                    fixed += 1
                    return m.group(1) + os.path.relpath(from_root, root) + m.group(3)
                return m.group(0)

            new = MD_LINK_RE.sub(repl, text)
            if new != text:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new)
    return fixed


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--src", required=True, help="GitBook space dir (holds SUMMARY.md)")
    ap.add_argument("--out", required=True, help="output dir (docs/ + mkdocs.yml)")
    ap.add_argument("--site-name", required=True)
    ap.add_argument("--site-url", default="", help="final public base URL (subpath-aware)")
    args = ap.parse_args()

    src, out = os.path.abspath(args.src), os.path.abspath(args.out)
    docs = os.path.join(out, "docs")
    if os.path.isdir(docs):
        shutil.rmtree(docs)
    os.makedirs(docs)

    converted = 0
    for root, dirs, files in os.walk(src):
        dirs[:] = [d for d in dirs if not d.startswith(".") or d == ".gitbook"]
        for name in files:
            if name.startswith(".") or name == "SUMMARY.md":
                continue
            src_path = os.path.join(root, name)
            rel = os.path.relpath(src_path, src)
            dst_path = os.path.join(docs, rel)
            os.makedirs(os.path.dirname(dst_path), exist_ok=True)
            if name.endswith(".md"):
                with open(src_path, encoding="utf-8") as f:
                    text = f.read()
                with open(dst_path, "w", encoding="utf-8") as f:
                    f.write(convert_page(text, rel))
                converted += 1
            else:
                shutil.copy2(src_path, dst_path)

    fixed = fix_root_relative_links(docs)
    if fixed:
        print("rewrote %d root-relative links" % fixed)

    summary_path = os.path.join(src, "SUMMARY.md")
    if not os.path.isfile(summary_path):
        raise ConvertError("SUMMARY.md not found in " + src)
    with open(summary_path, encoding="utf-8") as f:
        nav = build_nav(f.read())

    config = {
        "site_name": args.site_name,
        "docs_dir": "docs",
        "theme": {
            "name": "material",
            "language": "fr",
            "features": ["navigation.sections", "navigation.top", "toc.follow", "search.highlight"],
        },
        "markdown_extensions": [
            "admonition",
            "attr_list",
            "md_in_html",
            "tables",
            {"pymdownx.superfences": {}},
            {"pymdownx.tabbed": {"alternate_style": True}},
            # Unicode-preserving slugs so accented anchors (#déclarant) resolve.
            {"toc": {"permalink": True, "slugify": Raw("!!python/object/apply:pymdownx.slugs.slugify {kwds: {case: lower}}")}},
        ],
        "nav": nav,
    }
    if args.site_url:
        config["site_url"] = args.site_url

    with open(os.path.join(out, "mkdocs.yml"), "w", encoding="utf-8") as f:
        f.write("\n".join(dump_yaml(config)) + "\n")

    print("converted %d pages → %s" % (converted, out))


if __name__ == "__main__":
    try:
        main()
    except ConvertError as e:
        print("gitbook_to_mkdocs: " + str(e), file=sys.stderr)
        sys.exit(1)
