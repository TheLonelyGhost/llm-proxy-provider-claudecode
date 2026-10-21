# llm-proxy configuration for local end-to-end tests of the claudecode plugin.
#
# Before you use it, sideload the plugin and log in:
#   task sideload
#   llm-proxy plugin run claude login --config dev/llm-proxy.hcl
#
# Then start the proxy:
#   llm-proxy serve --config dev/llm-proxy.hcl

server {
  address = "127.0.0.1"
  port    = 14980

  # Debug body logging. Keep false unless you diagnose wire-level issues.
  log_bodies = false
}

plugin "claudecode" {
  source = "github.com/thelonelyghost/llm-proxy-provider-claudecode"
  # Exact version: `task sideload` installs the manifest version into the
  # plugin cache.
  version = "0.1.0"
}

backend "claude" {
  type = "claudecode"

  # Optional allow-list. Remove it to use every model of the account.
  models = ["haiku", "sonnet", "opus"]

  # Default reasoning effort when a request sets no reasoning_effort.
  effort = "low"

  max_concurrency = 2
}
