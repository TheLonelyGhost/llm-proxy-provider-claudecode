# llm-proxy-provider-claudecode

This plugin connects [llm-proxy](https://github.com/thelonelyghost/llm-proxy)
to Claude models through the official Claude Code CLI (`claude`). It uses the
Claude subscription that you use with Claude Code.

The plugin is an llm-proxy sidecar. llm-proxy starts it and sends
OpenAI-compatible chat requests to it. For each request, the plugin starts
`claude` in headless mode and returns the reply in OpenAI format.

## Compliance

Read this section before you use the plugin.

- Use the plugin only for yourself, on your own computer, with your own
  Claude account.
- Do not let other persons send requests through your proxy. The plugin and
  llm-proxy listen on `127.0.0.1` only. Do not change that.
- The plugin does not read, keep, or send your Claude credentials. The
  official `claude` CLI does the authentication.
- Anthropic terms apply to all use. For a Team or Enterprise plan, get
  approval from your organization administrator before you use the plugin.

## Requirements

- Claude Code (`claude`) 2.1.304 or later, on `PATH` or at `cli_path`.
- A Claude Pro, Max, Team, or Enterprise plan, or an Anthropic API key in
  `ANTHROPIC_API_KEY`.
- llm-proxy 1.1.0 or later. Earlier versions do not give the `backend`
  attributes to the sidecar, and they stop plugin replies after 60 seconds.

## Installation

Add a `plugin` block and a `backend` block to your llm-proxy configuration:

```hcl
plugin "claudecode" {
  source  = "github.com/thelonelyghost/llm-proxy-provider-claudecode"
  version = "~> 0.1"
}

backend "claude" {
  type = "claudecode"
}
```

Then install the plugin:

```bash
llm-proxy plugin install
```

Clients use the backend label as the model prefix, for example
`claude/sonnet`.

## Authentication

Log in to Claude Code before you start the proxy:

```bash
llm-proxy plugin run claude login
```

The `login` tool runs `claude auth login --claudeai`. Claude Code opens a
browser page for your Claude account. If Claude Code is already logged in,
the tool only shows the login method and the plan.

| Option | Effect |
|---|---|
| `--sso` | Use the SSO login flow. |
| `--email ADDR` | Put `ADDR` in the email field of the login page. |

Put `--` before the tool options, so that llm-proxy does not read them:

```bash
llm-proxy plugin run claude login -- --sso
```

The plugin keeps no credentials, so it has no `logout` tool. To log out, run
`claude auth logout`. This command logs out all of Claude Code on this
computer.

The plugin checks the login before each chat request. If Claude Code is not
logged in, the plugin returns HTTP 401 and sends nothing to the model. The
plugin keeps a good result for 60 seconds.

## Configuration

Set these attributes in the `backend` block. An environment variable in the
llm-proxy process overrides the attribute.

| Attribute | Variable | Default | Description |
|---|---|---|---|
| `models` | `CLAUDECODE_MODELS` | All models of your account | Models that clients can use. The variable is a comma-separated list. |
| `effort` | `CLAUDECODE_EFFORT` | The model default | Reasoning effort when a request does not set one. |
| `cli_path` | `CLAUDECODE_CLI_PATH` | `claude` | Path of the Claude Code CLI. |
| `max_concurrency` | `CLAUDECODE_MAX_CONCURRENCY` | `4` | Maximum number of chat requests that run at the same time. Other requests wait. |

llm-proxy gives the `backend` attributes to the plugin in
`LLM_PROXY_BACKEND_CONFIG`.

The plugin runs each `claude` process with these settings:

- No Claude Code tools.
- No settings files, hooks, plugins, MCP servers, or `CLAUDE.md` files.
- No slash commands. A message such as `/usage` goes to the model as text.
- No saved sessions.
- An empty temporary working directory.

## Available models

To show the models of your account, run:

```bash
llm-proxy plugin run claude list-models
```

The list contains the Claude Code aliases (for example `opus`, `sonnet`,
`haiku`) and the full model IDs (for example `claude-opus-5-5`). The list
does not use the `models` attribute.

`GET /v1/models` shows the `models` attribute if you set it. If not, it shows
the models of your account. A request for a model that is not in this list
gets HTTP 404 `model_not_found`.

## Reasoning effort

A request sets the effort with `reasoning_effort`. If the request does not
set it, the plugin uses the `effort` attribute. If neither is set, the model
uses its default.

| `reasoning_effort` | Claude Code effort |
|---|---|
| `none`, `minimal`, `low` | `low` |
| `medium` | `medium` |
| `high` | `high` |
| `xhigh` | `xhigh` |
| `max` | `max` |

Other values get HTTP 400. If a model does not support a level, Claude Code
uses the highest level below it. An Enterprise administrator can set a
maximum level.

## Budget

To show the usage of your plan, run:

```bash
llm-proxy plugin run claude budget
```

llm-proxy also shows this data at `GET /v1/usage/budget`. The tool sends no
message to the model.

- If your organization gives usage credits, `max_budget`, `spend`, and
  `remaining` show the credits in `currency` (for example `USD`).
- If not, `currency` is `percent`. `spend` shows the most-used plan limit
  window, from 0 to 100.
- `extras` shows each limit window, for example `session_percent` and
  `session_resets_at`.

Claude Code marks the usage data as experimental. If a Claude Code update
changes it, the tool shows an `error` object and exits with a non-zero code.

## Tool calling

Clients can send OpenAI `tools`. Claude can call them, but the client runs
them. The plugin does not run them.

1. The plugin gives the tools to Claude Code through an in-process MCP server.
2. When Claude calls a tool, the plugin stops the `claude` process.
3. The plugin returns `tool_calls` with `finish_reason: "tool_calls"`.
4. The client runs the tools and sends the results in `tool` messages.
5. The plugin starts a new `claude` process for the next reply.

`tool_choice` can be `auto` or `none`. Other values get HTTP 400.

## Limitations

- Only `POST /v1/chat/completions` is available. The plugin returns HTTP 501
  for `/v1/completions`, `/v1/embeddings`, and `/v1/responses`.
- The plugin gives earlier turns to Claude as text in one message. The plugin
  keeps no conversation data between requests.
- Messages can contain only text. Images and other content parts get HTTP 400.
- The plugin ignores `temperature`, `top_p`, `max_tokens`, `stop`, and other
  sampling parameters. Claude Code has no options for them.
- `n` must be 1. `response_format` must be `text`. The plugin does not
  support the `functions` parameter.
- Each request starts a new `claude` process. This adds some time to each
  request.

## Build from source

Requirements:

- Go 1.27.3 or later.
- [Task](https://taskfile.dev) v3.
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/) v2.

To build and install the plugin into the llm-proxy plugin cache, run:

```bash
task sideload
```

Use an exact `version` (for example `version = "0.1.0"`) in the `plugin` block
for a sideloaded plugin. `dev/llm-proxy.hcl` is an example configuration.

The build is static (`CGO_ENABLED=0`). It uses the Go FIPS 140-3 Cryptographic
Module. Run `llm-proxy-provider-claudecode version` to see the FIPS 140-3 mode.

## Development

Run `task quality-gate` before you open a pull request. The gate runs these
checks:

- `go mod tidy -diff`.
- golangci-lint with nilaway.
- govulncheck.
- A static FIPS build check.
- Tests with the race detector in strict FIPS mode.
- Tests in the release configuration (`CGO_ENABLED=0`) in strict FIPS mode.
- A minimum of 80% statement coverage.

`task package` makes the release archives and `SHA256SUMS` in `dist/`. A
`v*.*.*` tag starts the release workflow. The tag must agree with the
`version` in `manifest/llm-proxy-provider-claudecode.manifest.json`.
