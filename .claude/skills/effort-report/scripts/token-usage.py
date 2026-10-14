#!/usr/bin/env python3
"""Token + wall-clock totals from Claude Code transcripts, deduplicated.

Reads *.jsonl transcript paths as argv; prints a usage summary.

SCAR — dedupe by message.id or every number is ~2x too high.
A single API response is written to the transcript as MULTIPLE JSONL lines,
one per content block (text / thinking / each tool_use), and EVERY one of
those lines carries a full copy of the response's `usage` object. Summing
lines therefore multiplies each response by its content-block count. Measured
on this repo: 48,434 usage-bearing lines collapsed to 24,547 real responses,
a 1.97x inflation, and all 1,321 sampled duplicate groups had byte-identical
usage. message.id is unique per API response, so it is the correct key.
Duplicates are WITHIN a file, not across files — deduping per-file and summing
would not have caught it.
"""

import json
import sys

KEYS = (
    ("output", "output_tokens"),
    ("cache_read", "cache_read_input_tokens"),
    ("cache_creation", "cache_creation_input_tokens"),
    ("input", "input_tokens"),
)


def main(paths):
    totals = {k: 0 for k, _ in KEYS}
    seen, dupes, stamps = set(), 0, []

    for path in paths:
        try:
            fh = open(path, errors="ignore")
        except OSError:
            continue
        with fh:
            for line in fh:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                when = rec.get("timestamp")
                if when:
                    stamps.append(when)
                msg = rec.get("message") or {}
                usage = msg.get("usage")
                if not isinstance(usage, dict):
                    continue
                mid = msg.get("id")
                if not mid:
                    continue
                if mid in seen:
                    dupes += 1
                    continue
                seen.add(mid)
                for name, field in KEYS:
                    totals[name] += usage.get(field, 0)

    total = sum(totals.values())
    for name, _ in KEYS:
        pct = f"  ({100 * totals[name] / total:.2f}%)" if total else ""
        print(f"{name:21s} {totals[name]:>16,}{pct}")
    print(f'{"RAW TOTAL":21s} {total:>16,}')
    print(f'{"api responses":21s} {len(seen):>16,}')
    print(f'{"dup lines dropped":21s} {dupes:>16,}   <- content-block copies, not real calls')

    if not stamps:
        return
    # Timestamps are ISO-8601; lexical sort == chronological. Avoids a parse
    # per line on transcripts that run to six figures.
    stamps.sort()
    import datetime

    def ts(s):
        return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

    marks = [ts(s) for s in stamps]
    gap, active, sessions, start, prev = 1800, 0.0, 1, marks[0], marks[0]
    for mark in marks[1:]:
        if mark - prev > gap:
            active += prev - start
            sessions += 1
            start = mark
        prev = mark
    active += prev - start
    print(f'{"sessions":21s} {sessions:>16,}')
    print(f'{"wall-clock hours":21s} {active / 3600:>16,.1f}')
    print(f"transcript window     {stamps[0][:10]} -> {stamps[-1][:10]}")


if __name__ == "__main__":
    main(sys.argv[1:])
