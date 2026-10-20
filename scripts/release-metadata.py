"""Validate a release tag against the single tracked version source."""

import os
from pathlib import Path
import re

tag = os.environ["RELEASE_TAG"]
match = re.fullmatch(
    r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?",
    tag,
)
if not match:
    raise SystemExit("Tag must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-prerelease")
prerelease = match.group(4)
if prerelease and any(p.isdigit() and len(p) > 1 and p[0] == "0" for p in prerelease.split(".")):
    raise SystemExit("Numeric prerelease identifiers cannot have leading zeros")
if Path("VERSION").read_text().strip() != tag[1:]:
    raise SystemExit("Tag must match VERSION; update and commit VERSION before tagging")
with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
    output.write(f"version={tag[1:]}\n")
    output.write(f"prerelease={str(bool(prerelease)).lower()}\n")
