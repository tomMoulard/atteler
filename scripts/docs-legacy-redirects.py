#!/usr/bin/env python3
"""Create compatibility redirects from old unversioned docs URLs to /main/."""

from __future__ import annotations

import argparse
import html
import posixpath
import shutil
from pathlib import Path

LLM_DOCS = ("llms.txt", "llms-full.txt")


def redirect_html(target: str) -> str:
    escaped = html.escape(target, quote=True)
    js_target = target.replace("\\", "\\\\").replace('"', '\\"')

    return f"""<!DOCTYPE html>
<html>
<head>
  <meta charset=\"utf-8\">
  <title>Redirecting</title>
  <noscript>
    <meta http-equiv=\"refresh\" content=\"1; url={escaped}\" />
  </noscript>
  <script>
    window.location.replace(
      \"{js_target}\" + window.location.search + window.location.hash
    );
  </script>
</head>
<body>
  Redirecting to <a href=\"{escaped}\">{escaped}</a>...
</body>
</html>
"""


def redirect_target(from_dir: Path, site_root: Path, version: str, rel_dir: Path) -> str:
    destination = site_root / version / rel_dir
    target = posixpath.relpath(destination.as_posix(), start=from_dir.as_posix())
    if not target.endswith("/"):
        target += "/"

    return target


def write_redirects(site_root: Path, version: str) -> None:
    version_root = site_root / version
    if not version_root.is_dir():
        raise SystemExit(f"default docs version not found: {version_root}")

    for index in version_root.rglob("index.html"):
        rel_dir = index.parent.relative_to(version_root)
        if rel_dir == Path("."):
            continue

        redirect_dir = site_root / rel_dir
        redirect_dir.mkdir(parents=True, exist_ok=True)
        target = redirect_target(redirect_dir, site_root, version, rel_dir)
        (redirect_dir / "index.html").write_text(redirect_html(target), encoding="utf-8")

    for name in LLM_DOCS:
        source = version_root / name
        if source.is_file():
            shutil.copyfile(source, site_root / name)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("site_root", type=Path, help="Root of the built gh-pages tree")
    parser.add_argument("version", help="Default version directory, usually 'main'")
    args = parser.parse_args()

    write_redirects(args.site_root.resolve(), args.version)


if __name__ == "__main__":
    main()
