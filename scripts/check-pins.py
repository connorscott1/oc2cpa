#!/usr/bin/env python3
import json
from pathlib import Path
import re

root=Path(__file__).resolve().parents[1]
lock=json.loads((root/"UPSTREAM.lock").read_text())
mod=(root/"go.mod").read_text()
assert re.search(r"^go "+re.escape(lock["go_version"])+r"$",mod,re.M)
assert (root/".go-version").read_text().strip()==lock["go_version"]
assert "github.com/router-for-me/CLIProxyAPI/v8 "+lock["host_version"] in mod
assert not re.search(r"^replace\b",mod,re.M), "local dependency replacements cannot ship"
assert re.fullmatch(r"[a-f0-9]{40}",lock["reviewed_commit"])
assert re.fullmatch(r"[a-f0-9]{40}",lock["host_commit"])
registry=json.loads((root/"registry.json").read_text())["plugins"][0]
assert registry["repository"]=="https://github.com/connorscott1/oc2cpa"
assert registry["version"]==(root/"VERSION").read_text().strip()
assert registry["release_tag"]=="v"+registry["version"]
workflow=(root/".github/workflows/build.yml").read_text()
for action in re.findall(r"uses:\s*([^\s#]+)",workflow):
    assert re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[a-f0-9]{40}",action), action
assert "--clobber" not in workflow
assert "pull_request_target" not in workflow
print("Pinned toolchain, host, fork registry, action commits, and immutable-upload policy verified.")
