#!/usr/bin/env python3
"""Generate a GitHub-anchored Contents block from a doc's ## / ### headings."""
import re
import sys
import pathlib

MARK_OPEN = "<!-- toc -->"
MARK_CLOSE = "<!-- /toc -->"


def anchor(text: str) -> str:
    """GitHub's slug: strip inline markup, lowercase, drop punctuation, spaces->-."""
    t = re.sub(r"`([^`]*)`", r"\1", text)
    t = re.sub(r"\*\*([^*]*)\*\*", r"\1", t)
    t = re.sub(r"\*([^*]*)\*", r"\1", t)
    t = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", t)
    t = t.strip().lower()
    # Keep letters/digits/underscore/space/hyphen, plus the ZWJ and variation
    # selectors github-slugger never strips (so "\u26a0\ufe0f Heading" keeps its
    # invisible U+FE0F, as GitHub does).
    t = re.sub(r"[^\w\s\u200d\ufe00-\ufe0f-]", "", t, flags=re.UNICODE)
    # ONE HYPHEN PER SPACE, not a collapse of runs -- github-slugger does
    # .replace(/ /g, "-"), so " -- " and " / " leave DOUBLE hyphens behind.
    return t.replace(" ", "-")


def strip_markup(text: str) -> str:
    t = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)
    return t.strip()


def headings(body: str, max_level: int):
    out, fence = [], False
    for line in body.split("\n"):
        if line.startswith("```"):
            fence = not fence
            continue
        if fence:
            continue
        m = re.match(r"^(#{2,6})\s+(.*)$", line)
        if not m:
            continue
        lvl = len(m.group(1))
        if lvl > max_level:
            continue
        out.append((lvl, m.group(2).rstrip()))
    return out


def build(body: str, max_level: int) -> str:
    hs = headings(body, max_level)
    seen, lines = {}, [MARK_OPEN, "## Contents", ""]
    for lvl, text in hs:
        if text.lower() in ("contents", "see also"):
            continue
        a = anchor(text)
        n = seen.get(a, 0)
        seen[a] = n + 1
        if n:
            a = f"{a}-{n}"
        lines.append(f"{'  ' * (lvl - 2)}- [{strip_markup(text)}](#{a})")
    lines += ["", MARK_CLOSE]
    return "\n".join(lines)


def insert(path: pathlib.Path, max_level: int) -> str:
    body = path.read_text()
    toc = build(body, max_level)
    if MARK_OPEN in body:
        body = re.sub(
            re.escape(MARK_OPEN) + r".*?" + re.escape(MARK_CLOSE), toc, body, flags=re.S
        )
        path.write_text(body)
        return "refreshed"
    lines = body.split("\n")
    # After the title and any intro prose, immediately before the first ## heading.
    idx = next(
        (i for i, l in enumerate(lines) if re.match(r"^##\s", l)), len(lines)
    )
    while idx > 0 and lines[idx - 1].strip() == "":
        idx -= 1
    lines[idx:idx] = ["", toc]
    path.write_text("\n".join(lines))
    return "inserted"


if __name__ == "__main__":
    lvl = 3
    args = sys.argv[1:]
    if args and args[0].startswith("--level="):
        lvl = int(args[0].split("=")[1])
        args = args[1:]
    for f in args:
        print(f"{insert(pathlib.Path(f), lvl):9s} {f}")
