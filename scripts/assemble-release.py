#!/usr/bin/env python3
"""Validate all native artifact hashes and revisions before drafting a release."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

root=Path(__file__).resolve().parents[1]
revision=subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip()
version=(root/'VERSION').read_text().strip()
expected={'linux_amd64','linux_arm64','darwin_amd64','darwin_arm64','windows_amd64'}
out=root/'release-out'
out.mkdir(exist_ok=False)
seen=set()
checks=[]
for file in (root/'release-artifacts').rglob('*.build.json'):
    manifest=json.loads(file.read_text())
    assert manifest['source_commit']==revision and manifest['version']==version
    assert manifest['source_dirty'] is False
    target=manifest['target']['GOOS']+'_'+manifest['target']['GOARCH']
    assert target in expected and target not in seen
    seen.add(target)
    archive=file.parent/manifest['archive']
    assert hashlib.sha256(archive.read_bytes()).hexdigest()==manifest['archive_sha256']
    shutil.copy2(file,out/file.name)
    shutil.copy2(archive,out/archive.name)
    checks.append(manifest['archive_sha256']+'  '+archive.name)
assert seen==expected, (seen,expected)
(out/'checksums.txt').write_text('\n'.join(sorted(checks))+'\n')
shutil.copy2(root/'registry.json',out/'registry.json')
