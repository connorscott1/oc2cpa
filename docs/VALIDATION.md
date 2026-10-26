# Validation record — October 26, 2026

Target: CLIProxyAPI v8.0.44 (`1f917e77c4562623aa49e9804d999524151a2f96`),
matching the user's backend screenshot. Management Center v1.26.7 is the intended
UI; the dashboard was retained, and its browser interface has not been tested.

## Completed locally

- Ordinary Go tests, the race detector, vet, formatting and pin checks.
- Native macOS arm64 shared-library build, including deployment target and ABI
  export inspection, with Go 1.26.10.
- Actual native ABI calls rejecting null/oversized payloads and lengths above
  the signed C integer range before accessing their pointers.
- Real v8.0.44 host process against a confined loopback mock: native plugin load,
  built-in models, non-streaming and streaming inference, management-key
  enforcement, usage summary, and preservation of user credential fields after
  startup and configuration reload. The host may reformat credential JSON;
  field preservation is the regression criterion.

The initial integration test found an inherited configuration-parser bug:
flow-style YAML passed by the host was ignored and the default upstream was
used. Proper YAML decoding and fail-closed configuration validation were added.
The mock path now has an explicit loopback proxy that rejects external TLS
connections even if configuration regresses.

## Real free-tier inference

The explicit live test used runtime commit
`351c8277e69e8ed1278b886194259af3102901c4` (subsequent changes were to test/build
validation and documentation). It loaded the built plugin in the actual v8.0.44
host, used no personal provider credentials, and sent only a short prompt asking
for `OC2CPA_OK`, with an output limit of 64 tokens.

Endpoint: `https://opencode.ai/inference/openai/v1`.

| Model | Result |
| --- | --- |
| big-pickle | HTTP 429, Free usage limit reached |
| mimo-v2.6-flash-free | HTTP 429, Free usage limit reached |
| deepseek-v4.1-flash-free | HTTP 429, Free usage limit reached |
| glm-5.3-flash-free | HTTP 429, Free usage limit reached |

No successful live inference was established. The message also directs callers
to OpenCode or OpenCode Go, so it cannot be used to distinguish a genuine
exhausted IP allowance from client-access restrictions. There were no rate-limit
retries, IP rotation, account creation, or attempts to evade the rejection.
Streaming live inference was not attempted after a model's non-streaming request
was refused. Mock success does not substitute for real provider availability.

## Cleanup and deployment status

Every smoke-test host is terminated in a `finally` block; temporary auth,
configuration and plugin directories use `TemporaryDirectory` and are removed
on exit. The downloaded local host executable and archives are removed after
testing at the user's request. CI runners are disposable. No plugin was installed
in the user's running server and no release was published by these tests.

See the PR checks for final CI platform build/attestation status. Source and
artifact revisions/hashes are recorded in each build manifest; this record does
not claim all hosted runner images or generated binaries are reproducible.
