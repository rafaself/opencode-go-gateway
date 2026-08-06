# Security, privacy, and project boundary

OpenCode Gateway is a local, standard-library-only adapter. It is designed to
keep the Codex-facing listener on loopback and to keep the OpenCode Go
credential on the provider side of the process boundary.

## Data handling

- `OPENCODE_GO_API_KEY` is used only as the upstream authorization credential
  and is never written to Codex TOML, the model catalog, logs, fixture files,
  error bodies, or Responses bytes. An environment value takes precedence over
  the optional credential configured with `ocgtw config set-key`.
- On Linux, the configured credential is stored in Secret Service when the
  helper is available. The fallback is an owner-only `0700` directory and
  `0600` file containing the key in plaintext. That fallback is protected by
  filesystem permissions, not encryption; use the keyring or the environment
  workflow when stronger at-rest protection is required.
- `config status` reports only backend state. `config set-key` reads standard
  input and rejects command-line key values, reducing shell history and process
  listing exposure. `config remove-key` deletes the persistent value.
- Request bodies, prompts, instructions, source code, filesystem paths,
  environment values, client metadata, tool arguments, and provider reasoning
  are not logged. Capture output is redacted and must be reviewed before it is
  promoted to a fixture.
- Continuation state is in-memory and bounded. It is discarded on process
  exit; it is not persisted or shared between gateway processes.
- The `apply_patch` path transports a tool call but never executes a command,
  applies a patch, or inspects a user's filesystem.

## Network boundary

- The default listener is `127.0.0.1:8787`; non-loopback binding requires an
  explicit opt-in and does not provide authentication.
- Provider redirects and ambient proxy environment settings are disabled.
- Timeouts, request limits, stream limits, and continuation limits are finite.
- The capture server is development-only, loopback-only, and accepts only the
  contract endpoint. It must not be exposed to an untrusted network.

Do not place this service directly on a public interface. If a deployment
needs a remote client, put a separately reviewed authenticated and encrypted
network boundary in front of it, and treat that as a deployment change outside
the v0.1.0 release.

## Reporting a security issue

Do not put credentials, prompts, source code, or raw captures in a public issue
or pull request. Remove sensitive data and use the repository's private
maintainer contact or security reporting channel when one is available. If no
private channel is published, open a minimal issue that requests one without
including exploit material.

## Project disclaimer

OpenCode Gateway is an independent project. It is not OpenCode CLI, is not a
replacement for OpenCode CLI, and does not claim to be produced, sponsored, or
endorsed by OpenAI, OpenCode, DeepSeek, or the Codex CLI maintainers. Product
names and service names belong to their respective owners. The gateway only
uses a user's existing OpenCode Go subscription through its documented API
path; users remain responsible for provider terms, credentials, and costs.
