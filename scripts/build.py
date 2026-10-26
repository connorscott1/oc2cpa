#!/usr/bin/env python3
"""Build an exact clean revision; package its binary, hashes, and provenance."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import zipfile

ROOT = Path(__file__).resolve().parents[1]
os.chdir(ROOT)
def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
def main():
    subprocess.run(["python3", "scripts/check-pins.py"], check=True)
    if run("git", "status", "--porcelain", "--untracked-files=all"):
        raise SystemExit("Build requires a clean committed checkout; commit reviewed changes first.")
    revision = run("git", "rev-parse", "HEAD")
    version = Path("VERSION").read_text().strip()
    pin = Path(".go-version").read_text().strip()
    env = dict(os.environ, GOTOOLCHAIN="go"+pin, GOFLAGS="-mod=readonly", CGO_ENABLED="1")
    actual = run("go", "env", "GOVERSION", env=env)
    if actual != "go"+pin:
        raise SystemExit("Unexpected toolchain: "+actual)
    subprocess.run(["go", "mod", "verify"], env=env, check=True)
    target = json.loads(run("go", "env", "-json", "GOOS", "GOARCH", "CC", env=env))
    ext = {"linux":"so", "darwin":"dylib", "windows":"dll"}[target["GOOS"]]
    base = "oc2cpa_"+version+"_"+target["GOOS"]+"_"+target["GOARCH"]
    out = ROOT/"dist"/base
    out.mkdir(parents=True, exist_ok=True)
    library = out/("oc2cpa."+ext)
    if target["GOOS"] == "darwin":
        env["MACOSX_DEPLOYMENT_TARGET"] = "11.0"
    subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=true", "-buildmode=c-shared",
                    "-ldflags", "-s -w -X main.version="+version+" -X main.sourceCommit="+revision,
                    "-o", str(library), "."], env=env, check=True)
    if target["GOOS"] == "darwin":
        subprocess.run(["python3", ".github/scripts/check-darwin-dylib.py", str(library)], check=True)
    archive = out/(base+".zip")
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as z:
        entry = zipfile.ZipInfo(library.name, (1980,1,1,0,0,0))
        entry.external_attr = 0o100755 << 16
        entry.compress_type = zipfile.ZIP_DEFLATED
        z.writestr(entry, library.read_bytes())
    manifest = {
        "repository":"https://github.com/connorscott1/oc2cpa", "source_commit":revision,
        "version":version, "go_version":actual, "target":target,
        "host":json.loads(Path("UPSTREAM.lock").read_text()),
        "go_mod_sha256":digest(Path("go.mod")), "go_sum_sha256":digest(Path("go.sum")),
        "library":library.name, "library_sha256":digest(library),
        "archive":archive.name, "archive_sha256":digest(archive), "source_dirty":False,
        "compiler_version":run(target["CC"], "--version").splitlines()[0],
    }
    (out/(base+".build.json")).write_text(json.dumps(manifest,indent=2)+"\n")
    (out/"checksums.txt").write_text(digest(archive)+"  "+archive.name+"\n")
    print(json.dumps(manifest,indent=2))
if __name__ == "__main__":
    main()
