Security fixes and pinned builds for connorscott1/oc2cpa.

Preserves existing credentials, validates proxies without silent fallback,
omits sensitive error content from logs, hashes client usage labels, fixes
model-discovery/reload locking, and bounds native/request/response sizes and
upstream durations. The dashboard remains enabled.

Targets CLIProxyAPI v8.0.44 with Go 1.26.10. Each archive has a build manifest and
SHA-256 checksum; verify its source commit and GitHub provenance before use.
CI creates draft releases only and refuses to replace existing assets.

See docs/MAINTENANCE.md for verification, review/update procedure, supported
platforms, and remaining native-plugin and cancellation limitations. No real
provider account, subscription, or unlimited allowance is included.
