# Maintaining this fork

The upstream baseline, host release/commit, and Go patch version are recorded in
`UPSTREAM.lock`. The current compatibility target is CLIProxyAPI **v8.0.44**
(`1f917e77c4562623aa49e9804d999524151a2f96`), with the user's Management Center
v1.26.7. The dashboard remains enabled; this fork does not remove its access to
the saved management key. UI interactions are not covered by the mock test.

## Review updates before applying them

1. Resolve a proposed upstream change to its full 40-character commit. Run
   `python3 scripts/review-upstream.py <commit>` to fetch and inspect its diff.
   This command never merges or changes the reviewed baseline.
2. Inspect credential/file mutations, destinations and proxy behavior, diagnostic
   content, native boundaries, resource limits, browser assets, dependencies,
   build actions, and release permissions. Port selected changes onto a review
   branch; do not automatically reset this fork to upstream.
3. Update `UPSTREAM.lock` only after reviewing the candidate, preserving the
   fork's regression tests. A host upgrade also requires reviewing the plugin
   API/ABI and updating the dependency, checksum, lock, and pinned smoke-host
   archive/hash together. Commit the reviewed changes.
4. Run the pin checks, ordinary tests, race detector, vet, a native build, and
   `scripts/smoke-host.py` against the exact intended host release. Verify both
   streaming and aggregate responses, management authentication, existing
   credential preservation, and reload behavior. Test live OpenCode inference
   separately before installation: the mock does not establish upstream access. Run the explicitly opt-in
   `python3 scripts/smoke-host.py --download --live` for a benign live free-tier
   check; it records provider rejections honestly and does not rotate IPs or
   retry rate-limit responses. Each temporary host/auth directory is deleted
   and the process stopped on success or failure.
5. Merge only after the results and any remaining limitations are reviewed.
   No scheduled syncing, automatic merging, dependency update bot, or automatic
   installation is configured.

## Build a pinned artifact

Use a clean checkout of an exact reviewed commit, not an uncommitted working tree:

```sh
git checkout <reviewed-commit>
python3 scripts/check-pins.py
go test -mod=readonly ./...
go test -mod=readonly -race ./...
go vet -mod=readonly ./...
python3 scripts/build.py
python3 scripts/smoke-abi.py
python3 scripts/smoke-host.py --download
```

The script selects Go 1.26.10, requires a clean tree, disallows module changes,
verifies cached modules, and builds with VCS metadata and trimmed source paths.
Each target's `dist/` folder contains the native library, a single-library zip,
SHA-256 checksum, and a build manifest with source revision, toolchain, module
hashes, compiler identity, and binary/archive hashes. The manifest is provenance
information, not an independently trusted signature.

CI pins checkout, setup, artifact, and attestation actions to full commits. It
builds on native Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 runners.
Windows ARM64 is deliberately not packaged: the old unpinned third-party cross
build is removed and has not been replaced with a validated toolchain. Hosted
runner OS/compiler images still change; their recorded identity does not make
builds bit-for-bit reproducible. Linux builds must be checked against the target
server's libc before installation.

Push builds request GitHub provenance attestations for zip artifacts. PR builds
run without release publication and do not request provenance attestations.
Verify an attested archive with `gh attestation verify <archive> --repo
connorscott1/oc2cpa`, and separately compare its checksum and manifest source
commit to the revision you reviewed. CI artifact URLs refer to workflow runs;
retain your approved archive/hash for installation.

## Releases and installation

A version tag must match `VERSION` and `registry.json`. Tag builds may create a
**draft** release only after every platform succeeds. An existing release causes
failure; the workflow never replaces its assets. Publishing the draft is a
separate reviewed step. Enable GitHub immutable releases before publishing if
your repository supports it; this setting is not changed by the workflow.

Use a version-specific registry URL, for example
`https://github.com/connorscott1/oc2cpa/releases/download/v0.4.4-connorscott.1/registry.json`.
Do not use a floating `releases/latest` or upstream store source for this fork.
This document is preparation, not evidence that this version is already released
or installed. User approval/testing precedes installation.

## Runtime changes and remaining boundaries

- Block-style YAML, flow-style YAML, and JSON configuration are decoded with
  pinned yaml.v3 v3.0.1. Invalid settings fail closed instead of silently
  replacing a configured upstream with the default.
- Existing credential names/providers are detected through the host envelope;
  the plugin's own bootstrap calls are serialized and do not intentionally save
  over existing entries. The host API has no atomic create-only flag: concurrent
  external credential edits still require operator coordination.
- Explicit HTTP, HTTPS, SOCKS5, and SOCKS5H proxies are supported. Malformed or
  unsupported proxies fail instead of falling back. With no explicit/global
  proxy, Go's normal environment proxy behavior remains available.
- Raw upstream error bodies/messages are omitted from logs. Client usage labels
  are hashes instead of key prefixes. Upstream error text returned to the caller
  remains diagnostic content and must not be treated as instructions.
- Limits: 128 MiB RPC envelopes, 32 MiB incoming chat payloads, 8 MiB SSE
  lines/events, 64 MiB total inference response bytes, 2 MiB model catalogs.
  Errors are returned rather than silently truncating data.
- `request_timeout_ms` bounds the whole upstream HTTP operation: 600000 ms by
  default, configurable from 1 to 1800000 ms. Catalog discovery also has a
  15-second deadline. Existing idle watchdogs remain. Disconnect cancellation
  is observed on host stream emission; the plugin cannot instantly cancel a
  silent direct HTTP request from this ABI. The total deadline bounds it.
- Native request/response lengths are checked before C integer conversion, and
  request/stream panics are contained where recoverable. Native memory faults,
  runtime failures, and overall host memory exhaustion cannot be isolated by Go
  recovery. Keep a separate restricted instance for initial deployment.
- The dashboard's management-key access and unauthenticated static page remain
  as accepted by the user. Do not share URLs containing management keys. Model
  and upstream session behavior otherwise remain inherited from the baseline.
