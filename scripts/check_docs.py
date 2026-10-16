#!/usr/bin/env python3
"""Check repository Markdown/HTML local links and heading anchors; no network."""

import html
from html.parser import HTMLParser
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit


ROOT = Path(__file__).resolve().parents[1]
INLINE_LINK = re.compile(
    r"!?\[(?:\\.|[^\]\\\n])*\]\(\s*"
    r"(<[^>\n]+>|(?:\\.|[^()\s]|\([^()\s]*\))+)"
    r"(?:\s+(?:\"[^\"\n]*\"|'[^'\n]*'))?\s*\)"
)
REFERENCE = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*(<[^>]+>|\S+)")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")
HEADING = re.compile(r"^ {0,3}#{1,6}(?:\s+|$)(.*?)(?:\s+#+)?\s*$")
INLINE_CODE = re.compile(r"(`+).*?\1")


class HTMLLinks(HTMLParser):
    def __init__(self):
        super().__init__()
        self.links = []
        self.ids = set()

    def handle_starttag(self, tag, attrs):
        for key, value in attrs:
            if value is None:
                continue
            if key in {"href", "src"}:
                self.links.append((self.getpos()[0], value))
            if key == "id" or tag == "a" and key == "name":
                self.ids.add(value)


def prose_lines(text):
    """Keep line numbers while excluding fenced code and its example links."""
    fence = None
    for number, line in enumerate(text.splitlines(), 1):
        match = FENCE.match(line)
        if match:
            marker = match[1]
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence) and not line[match.end():].strip():
                fence = None
            yield number, ""
        else:
            yield number, line if fence is None else ""


def slug(title):
    title = re.sub(r"<[^>]*>", "", title)
    title = INLINE_LINK.sub(lambda match: match[0].split("](", 1)[0].lstrip("!["), title)
    title = html.unescape(title).strip().lower()
    return re.sub(r"[^\w\- ]", "", title).replace(" ", "-")


def inspect_document(path):
    text = path.read_text(encoding="utf-8")
    if path.suffix.lower() == ".html":
        parser = HTMLLinks()
        parser.feed(text)
        return parser.links, parser.ids

    links, anchors, previous = [], set(), ""
    lines = list(prose_lines(text))
    for number, line in lines:
        plain = INLINE_CODE.sub("", line)
        links.extend((number, match[1].strip("<>")) for match in INLINE_LINK.finditer(plain))
        reference = REFERENCE.match(plain)
        if reference:
            links.append((number, reference[1].strip("<>")))
        heading = HEADING.match(line)
        title = heading[1] if heading else None
        if re.fullmatch(r" {0,3}(?:=+|-+)\s*", line) and previous.strip():
            title = previous.strip()
        if title is not None:
            base = slug(title)
            anchor, count = base, 0
            while anchor in anchors:
                count += 1
                anchor = f"{base}-{count}"
            anchors.add(anchor)
        previous = line
    parser = HTMLLinks()
    parser.feed("\n".join(INLINE_CODE.sub("", line) for _, line in lines))
    links.extend(parser.links)
    anchors.update(parser.ids)
    return links, anchors


def documents(root):
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.md", "*.html"],
        cwd=root, check=True, stdout=subprocess.PIPE,
    )
    return sorted({root / item.decode() for item in result.stdout.split(b"\0") if item and (root / item.decode()).is_file()})


def check_link(root, source, target, cache):
    target = html.unescape(re.sub(r"\\([()])", r"\1", target))
    try:
        parts = urlsplit(target)
    except ValueError:
        return "invalid link"
    if parts.scheme or parts.netloc:
        return None
    relative = unquote(parts.path)
    destination = source
    if relative:
        destination = root / relative.lstrip("/") if relative.startswith("/") else source.parent / relative
        destination = destination.resolve()
    try:
        destination.relative_to(root)
    except ValueError:
        return "target escapes repository"
    if not destination.exists():
        return "target does not exist"
    if not parts.fragment:
        return None
    if destination.is_dir():
        destination /= "README.md"
    if destination.suffix.lower() not in {".md", ".html"}:
        return None
    if not destination.is_file():
        return "anchor target does not have a README"
    if destination not in cache:
        cache[destination] = inspect_document(destination)
    if unquote(parts.fragment) not in cache[destination][1]:
        return "heading or HTML anchor does not exist"
    return None


def main():
    cache = {}
    failures, count = [], 0
    paths = documents(ROOT)
    for path in paths:
        if path not in cache:
            cache[path] = inspect_document(path)
        for line, target in cache[path][0]:
            error = check_link(ROOT, path, target, cache)
            if error:
                failures.append(f"{path.relative_to(ROOT)}:{line}: {error}: {target}")
            count += 1
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"Documentation links OK: {len(paths)} files, {count} links (external URLs not fetched).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
