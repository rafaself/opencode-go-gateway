# Codex setup and diagnostics

`opencode-gateway setup codex` configures the user-level Codex configuration
for the local OpenCode Gateway. It never edits a project `.codex` file and it
never stores `OPENCODE_GO_API_KEY`.

```bash
opencode-gateway setup codex
opencode-gateway doctor
```

The setup command resolves the Codex home from `CODEX_HOME` when it is set;
otherwise it uses the platform user home and `.codex` (`~/.codex` on Unix).
Relative `CODEX_HOME` values are rejected so the current working directory can
never become the destination accidentally. Tests and isolated environments can
pass `--codex-home /absolute/path`.

The command manages two provider tables in `config.toml`:
`[model_providers.opencode-gateway-go]`, which routes Codex to the local
gateway instance bound to the OpenCode Go endpoint, and
`[model_providers.opencode-gateway-zen]`, which routes to the second gateway
instance bound to the OpenCode Zen endpoint. The Zen backend serves both paid
and free Zen models; free models are not a separate backend, they are model
IDs selected by profile. The two gateway session profiles activate the
backends: `codex --profile opencode-gateway-go` runs the paid DeepSeek V4
Flash model, and `codex --profile opencode-gateway-zen-free` runs
`deepseek-v4-flash-free` through the Zen backend. Existing tables, unknown
settings, and comments remain in place where the TOML shape permits safe
editing. The superseded `[model_providers.opencode-gateway]` table from
earlier setup revisions is removed so orphaned credentials and dead routing
paths are not retained.

The generated `models.json` is written next to the Codex config and contains
the catalog metadata used by Codex for both models: text-only input, a
1,048,576-token context window, low/high/max reasoning, parallel function
tools, freeform `apply_patch`, no WebSocket transport, and the required
`base_instructions` and `experimental_supported_tools` fields. The
`deepseek-v4-flash-free` entry is the free Zen model; free models are
time-limited and their data may be used to improve the model.

Before a change, setup creates a timestamped `backup-opencode-gateway-*`
directory containing the previous managed files and a manifest. Both target
files are staged, parsed/validated, and atomically replaced; a replacement
failure rolls back files already replaced. New and updated files use owner-only
permissions. Re-running setup with the same gateway URL is a no-op.

Preview a change without creating a directory, backup, or file:

```bash
opencode-gateway setup codex --dry-run
```

The preview intentionally reports only managed operation names and redacted
values. Restore a setup backup with the exact path printed by setup:

```bash
opencode-gateway setup codex --restore /absolute/path/to/backup-opencode-gateway-...
```

The restore path must be a backup beneath the selected Codex home and the
backup files must pass the same TOML/JSON validation before replacement.

## Doctor checks

`opencode-gateway doctor` reports `PASS`, `WARN`, or `FAIL` checks and returns
exit code `1` when a failure is found. It checks the local gateway health
endpoints, loadability of the gateway environment, presence of the upstream
key without printing it, the configured provider and retry policy, Codex
config/catalog syntax and permissions, a safe `/v1/models` authentication
probe, model availability, and the Codex executable version. HTTP 429 and a
missing Codex executable are warnings because they identify an environment
condition without proving that the local configuration is unsafe; malformed
configuration, unavailable health endpoints, authentication failures, server
failures, and a missing `deepseek-v4-flash` are failures. A missing
`deepseek-v4-flash-free` is a warning because the free model can be
unavailable independently of the local configuration.

The provider probe is deliberately not an inference request. It uses the
configured OpenCode Go credential, preferring `OPENCODE_GO_API_KEY` and then
the value saved with `ocgtw config set-key`. When no credential is available,
setup and all offline validation remain usable, but a real provider
authentication/model check cannot pass.

The generated fields follow the current [Codex configuration
reference](https://developers.openai.com/codex/config-reference), [custom
provider guidance](https://developers.openai.com/codex/config-file/config-advanced),
and [DeepSeek's Codex integration
reference](https://api-docs.deepseek.com/quick_start/agent_integrations/codex/).
