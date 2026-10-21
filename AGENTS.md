# AGENTS.md

Guidance for AI coding agents in this repository.

## Layout

| Path | Owns |
| --- | --- |
| `cmd/llm-proxy-provider-claudecode/main.go` | Process entrypoint only: signal context, exit code. Keep it small. |
| `internal/cli` | Command tree (stdlib `flag`), usage text, exit codes. No args runs the sidecar; `--tool <name>` runs a tool. |
| `internal/config` | Backend config: `LLM_PROXY_BACKEND_CONFIG` JSON, then `LLM_PROXY_BACKEND_LABEL`, then `CLAUDECODE_*` overrides. |
| `internal/claudecli` | The only code that runs `claude`: isolation flags, stream-json protocol, tool bridge (in-process MCP), auth status, login, catalogue, usage. |
| `internal/chat` | OpenAI chat request/response translation, SSE chunks, error envelope. |
| `internal/server` | Sidecar HTTP routes, login gate, model check, concurrency limit. |
| `internal/tool` | `login`, `list-models`, `budget` tools. |
| `manifest/` | llm-proxy plugin manifest. Its `version` is the release version; its `tools` must equal `tool.Names`. |
| `assets/budget.schema.json` | Contract for `--tool budget` output (copied from llm-proxy). |
| `Taskfile.yml` | Every build, test, and lint entrypoint. CI and people use the same targets. |
| `.golangci.yml`, `.custom-gcl.yml` | Lint config and the custom golangci-lint build that bundles nilaway. |

## Commands

Use Task, not ad hoc `go` commands, so that flags and environment stay the same:

```bash
task build          # ./bin/llm-proxy-provider-claudecode (CGO_ENABLED=0, GOFIPS140 module)
task build:verify   # fail unless the binary is static and uses the FIPS 140-3 module
task run -- ARGS    # run from source
task test           # go test -race, GODEBUG=fips140=only, coverage profile
task test:release   # go test with CGO_ENABLED=0, GODEBUG=fips140=only
task coverage       # test + enforce >= 80% total statement coverage
task lint           # custom golangci-lint (standard linters + nilaway)
task fmt            # gofmt + goimports via the custom linter
task vuln           # govulncheck
task quality-gate   # tidy + lint + vuln + build:verify + test:release + coverage
task package        # release archives + SHA256SUMS in dist/
task sideload       # install bin + manifest into llm-proxy's plugin cache
```

## Rules

- `task quality-gate` must pass before a change is complete. Do not lower
  `COVERAGE_MIN`, disable a linter, or add `//nolint` without a concrete reason
  (nolintlint enforces the reason).
- Release builds use `CGO_ENABLED=0`. Only the `test` task sets `CGO_ENABLED=1`,
  because the race detector needs `cgo` on Linux. `test:release` runs the tests
  again without `cgo`. Do not add a `cgo` dependency without approval. Prefer a
  pure Go module.
- Each Go command in `Taskfile.yml` sets its environment inline. Do not move the
  values into a Task `env:` block: a shell value would override it.
- Builds use the Go FIPS 140-3 module (`GOFIPS140`). Tests run with
  `GODEBUG=fips140=only`, so a non-approved algorithm fails the test. Replace the
  algorithm. Do not remove the setting.
- Use only the standard library `crypto/...` packages for cryptography. Do not add
  a third-party cryptography module.
- stdout carries command output only. Send prompts, progress, and errors to stderr.
- Never log secrets.
- Keep dependencies lean. Justify each new module in the pull request description.
- Test through seams, not the network. Inject clocks, environment access, and
  clients. Use `t.TempDir()` for files.
- Exit codes: `0` success, `1` runtime failure, `2` usage error (prints usage to stderr).

## Plugin rules

- Follow the llm-proxy plugin contract (`docs/plugins/authoring.md` in
  llm-proxy): bind `127.0.0.1:$LLM_PROXY_PLUGIN_PORT` only, `/healthz` 200,
  `POST /management/reload` 2xx, 501 (never 404) for unsupported OpenAI
  endpoints, exit cleanly on SIGTERM.
- Never read, store, log, or forward Claude credentials, OAuth tokens, the
  Keychain, or `~/.claude/.credentials.json`. Authentication belongs to the
  unmodified `claude` binary (`claude auth login`). Do not spoof client
  headers or call Anthropic APIs directly with a subscription token.
- Keep the isolation flags in `claudecli` (`--tools ""`, `--setting-sources ""`,
  `--strict-mcp-config`, `--disable-slash-commands`, `--no-session-persistence`,
  `--permission-prompts none`, empty work dir). Never add
  `--permission-mode bypassPermissions`, `--dangerously-skip-permissions`, or
  `--bare` (it disables subscription OAuth).
- Client tools are never executed by the plugin. The bridge never answers
  `tools/call`; the run stops at `tool_use`.
- No message may reach the model before the login check and the model check
  pass (`server.handleChat`). Keep a test for that order.
- Stateless: persist nothing. Use temp dirs that are removed on exit.
- Never log request or response bodies, prompts, or account identifiers
  (email, organization).
- Docs move with the code: README.md for operator-visible behavior, the
  manifest for tools.

## Changelog

Maintain `CHANGELOG.md` for every end-user facing change.

- All changes go under `## [Unreleased]` until a version tag is cut.
- Use Keep a Changelog headings: `Added`, `Changed`, `Deprecated`, `Removed`,
  `Fixed`, `Security`.
- One line per change. State what changed and what it means to the operator;
  omit implementation detail. Keep only what an operator can observe or act on
  without reading the source.
- Breaking changes get `**BREAKING:**` in the bullet and a
  `### Breaking Changes` summary at the top of the version section.
- Internal-only changes (refactors, tests, CI, doc-only) need no entry.
- To release, use `.agents/skills/cut-release`: it moves `[Unreleased]` into a
  dated section and bumps the manifest version; the tag triggers
  `.github/workflows/release.yml`.

## Add a subcommand

1. Add a `case` in `cli.(*App).dispatch`.
2. Parse flags with `flag.NewFlagSet(..., flag.ContinueOnError)`. Return
   `usageError` for bad input so that the exit code is 2.
3. Put the behavior in an `internal/<area>` package behind an interface that the CLI can fake.
4. Update the `usage` string, README.md, and tests. Run `task quality-gate`.
