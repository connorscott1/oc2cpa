#!/usr/bin/env python3
"""Fetch a candidate for inspection only; never merge, checkout, or update pins."""
import json
from pathlib import Path
import re
import subprocess
import sys
root=Path(__file__).resolve().parents[1]
lock=json.loads((root/"UPSTREAM.lock").read_text())
if len(sys.argv)!=2 or not re.fullmatch(r"[a-f0-9]{40}",sys.argv[1]):
    raise SystemExit("Usage: python3 scripts/review-upstream.py <full-candidate-commit>")
sha=sys.argv[1]
subprocess.run(["git","fetch","--no-tags",lock["repository"],sha],cwd=root,check=True)
subprocess.run(["git","diff","--stat",lock["reviewed_commit"],sha],cwd=root,check=True)
subprocess.run(["git","diff",lock["reviewed_commit"],sha,"--"],cwd=root,check=True)
print("Inspection only: working tree, review baseline, and dependencies were not updated.")
