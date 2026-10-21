# Changelog

All notable end-user facing changes to `llm-proxy-provider-claudecode` are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added

- llm-proxy sidecar that sends OpenAI-compatible chat requests (streaming and non-streaming) to Claude models through the official Claude Code CLI and your Claude subscription.
- `login` tool (`llm-proxy plugin run <label> login [-- --sso --email ADDR]`) that runs `claude auth login`; chat requests get HTTP 401 and send nothing to the model until Claude Code is logged in.
- `list-models` tool and `GET /v1/models` that show the models of your Claude account; the `models` attribute (`CLAUDECODE_MODELS`) limits the models that clients can use.
- `reasoning_effort` on chat requests and the `effort` attribute (`CLAUDECODE_EFFORT`) set the Claude Code reasoning effort.
- `budget` tool that shows usage credits or plan limit usage for `GET /v1/usage/budget`.
- OpenAI tool calling: Claude can call client `tools`, and the plugin returns `tool_calls` for the client to run.
- Multi-turn conversations: earlier messages, tool calls, and tool results go to Claude as real conversation turns, so Claude Code can reuse its prompt cache for them.
- Message text goes to the model as written: `/commands` and `@file` mentions in a message have no effect.
- User and tool messages can contain images (`image_url` with a base64 data URL or an `https` URL) and PDF files (`file` with `file_data`); Claude gets them as images and documents. Audio, `file_id`, and other file types get HTTP 400.
- `cli_path` (`CLAUDECODE_CLI_PATH`) and `max_concurrency` (`CLAUDECODE_MAX_CONCURRENCY`) attributes.

[Unreleased]: https://github.com/thelonelyghost/llm-proxy-provider-claudecode/commits/main
