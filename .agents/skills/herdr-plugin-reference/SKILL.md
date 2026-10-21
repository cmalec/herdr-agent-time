---
description: |
  Reference for Herdr plugin manifests, runtime environment, lifecycle, pane metadata tokens, sidebar custom tokens, and the socket API. Use when changing herdr-plugin.toml, plugin install/update/stop behavior, Herdr socket calls, metadata tokens, or sidebar layout instructions. Covers Herdr 0.9.x and links to the full docs.
name: herdr-plugin-reference
---

# Herdr Plugin Reference

Use this skill for Herdr plugin work in this repository. It records the parts of
the Herdr contract this plugin depends on. Read the linked full docs before
using APIs that this plugin does not use.

## Sources

- Plugins: <https://herdr.dev/docs/plugins/>
- Socket API: <https://herdr.dev/docs/socket-api/>
- Configuration and sidebar: <https://herdr.dev/docs/configuration/>
- CLI reference: <https://herdr.dev/docs/cli-reference/>
- Herdr 0.9.5 source: <https://github.com/herdrdev/herdr/tree/v0.9.5>

## Manifest

`herdr-plugin.toml` is the plugin contract. Required top-level fields are `id`,
`name`, `version`, and `min_herdr_version`. `description` and `platforms` are
optional, but a local plugin without `platforms` links with a warning.

Commands are argv arrays. Herdr does not use a shell, so there is no glob or
variable expansion unless the command starts a shell.

Entrypoints:

- `[[build]]` runs during a GitHub install. `plugin link` does not run it.
- `[[startup]]` runs after session restore and socket readiness. It runs again
  on live handoff, but not on client attach, config reload, link, or enable.
- `[[actions]]` are invoked by id or keybinding. Action ids may not contain dots.
- `[[events]]` run for the event names listed in `PLUGIN_HOOK_EVENT_KINDS`.
- `[[panes]]` open terminal panes. Placement is overlay, popup, split, tab, or
  zoomed. There is no hidden placement.
- `[[link_handlers]]` route modified clicks on matching URLs to an action.

Plugin ids may contain ASCII letters, digits, dot, colon, underscore, and
hyphen. Herdr qualifies action ids as `plugin.id.action`.

## Lifecycle rules

These rules matter for this plugin:

- A startup hook is a one-shot command, not a supervised daemon. It should
  restore state, call needed APIs, and exit.
- Herdr does not supervise processes a plugin leaves behind. The docs require a
  plugin to stop detached background processes before its parent command or
  pane exits.
- `plugin update` does not restart existing processes. New invocations use the
  new installation, but a running daemon keeps the old binary.
- `plugin.disable` changes the registry flag and clears the plugin agent view.
  It does not kill plugin processes.
- `plugin.unlink` removes the registry entry and plugin pane records. It does
  not kill processes. `plugin install` over a linked plugin is refused.
- `plugin uninstall` unregisters the plugin and retains files for running
  processes. It does not run an uninstall hook.

Because there is no uninstall hook, a detached plugin daemon must detect that
it is no longer wanted. Use `plugin.list` to check `enabled`, `plugin_root`, and
presence. An identity file with the executable path and hash lets `ensure`
replace a daemon after an update or relink.

## Runtime environment

Herdr injects:

| Variable                                              | Meaning                                                     |
| ----------------------------------------------------- | ----------------------------------------------------------- |
| `HERDR_SOCKET_PATH`                                   | Running server socket. Required by this plugin.             |
| `HERDR_BIN_PATH`                                      | Running Herdr executable; preferred for portable CLI calls. |
| `HERDR_ENV=1`                                         | Set when running under Herdr.                               |
| `HERDR_PLUGIN_ID`                                     | Current plugin id.                                          |
| `HERDR_PLUGIN_ROOT`                                   | Installed or linked plugin directory.                       |
| `HERDR_PLUGIN_CONFIG_DIR`                             | User-editable plugin config directory.                      |
| `HERDR_PLUGIN_STATE_DIR`                              | Durable runtime state directory.                            |
| `HERDR_PLUGIN_CONTEXT_JSON`                           | Invocation context JSON.                                    |
| `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`, `HERDR_PANE_ID` | Focused context when available.                             |
| `HERDR_PLUGIN_ACTION_ID`                              | Action invocation.                                          |
| `HERDR_PLUGIN_EVENT`, `HERDR_PLUGIN_EVENT_JSON`       | Event hook invocation.                                      |
| `HERDR_PLUGIN_ENTRYPOINT_ID`                          | Pane invocation.                                            |

Do not store durable user data in `HERDR_PLUGIN_ROOT`. GitHub installs replace
that directory. Use the state and config directories.

## Socket protocol

The socket is newline-delimited JSON. One request per line:

```json
{ "id": "req_1", "method": "agent.list", "params": {} }
```

One response per line:

```json
{"id":"req_1","result":{"agents":[]}}
{"id":"req_1","error":{"code":"bad_request","message":"..."}}
```

Use `herdr api schema --json` for the full bundled schema. Protocol 22 ships with
Herdr 0.9.5. Reuse one connection for request/response work. Use CLI wrappers
through `HERDR_BIN_PATH` when portability matters more than latency.

Methods used by this plugin:

- `agent.list` with empty params returns `{agents: [...]}`.
- `plugin.list` with empty params returns `{plugins: [...]}`.
- `pane.report_metadata` writes display metadata and tokens.

Useful `AgentInfo` fields: `pane_id`, `agent`, `agent_status`,
`state_change_seq`, `completion_seq`, `terminal_id`, `workspace_id`, `tab_id`,
`tokens`, and `agent_session`. Status values are `idle`, `working`, `blocked`,
`done`, and `unknown`.

## Pane metadata and tokens

`pane.report_metadata` is display-only. It does not change semantic agent state.

Params this plugin uses:

```json
{
  "pane_id": "w1:p1",
  "source": "herdr-agent-time",
  "tokens": { "turn_time": "1:42" },
  "ttl_ms": 3000
}
```

Rules:

- Token maps are patches. A string sets a key. JSON `null` clears a key.
  Omitted keys are unchanged.
- `ttl_ms` is optional and must be 1 through 86400000 milliseconds. Omit it for
  a value that stays until replaced, cleared, or pane close.
- TTL applies to each token key updated by that call. Token metadata is not
  restored after a server restart.
- A report may mention at most 16 token keys. A pane retains at most 32 keys.
- Token names are 1-32 ASCII letters, digits, underscores, or hyphens.
- `source` is 80 characters or fewer and uses ASCII letters, digits, colon,
  dot, underscore, hyphen.
- `seq` is optional. Stale sequence numbers from the same source are accepted
  but ignored. Each pane accepts sequenced reports from at most 32 sources.
- Values are trimmed and capped at 80 characters. Control characters are
  removed. An empty value clears the key.
- Token changes and TTL expiry emit `pane.updated`. Plugin event hooks do not
  receive high-volume events such as `pane.updated`.

For a live counter, use a short TTL and refresh it. For a completed value, omit
the TTL. Never add a TTL to a completed value unless you want it to disappear.

## Sidebar custom tokens

Custom pane tokens render as `$name` in Agent sidebar rows:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", "workspace", "tab"],
  ["agent", "$turn_time"],
]
```

Rules:

- `rows` is a list of rows. Each row is a list of built-in tokens, `$name`
  tokens, or inline style tables. Limits are 16 rows and 16 tokens per row.
- Missing values and their separators disappear. A row disappears when every
  token in it is missing.
- `[ui.sidebar.agents.rows_by_agent]` overrides replace `rows` for that agent;
  they do not extend it. Add `$name` to each override that should show it.
- `rows_by_agent` keys are canonical agent ids such as `claude`, `codex`, and
  `pi`, not aliases.
- Rules can style or hide values by text or number. Regex, fuzzy matching, and
  scripts are not supported.
- Herdr does not expose a socket API to edit config. A plugin must instruct the
  user or an agent to edit `config.toml`, then run `herdr server reload-config`.

`herdr config check` validates `config.toml` and honors `HERDR_CONFIG_PATH`.
Run it after a manual edit.

## This repository

- Plugin id: `cmalec.agent-time`.
- Source: `herdr-agent-time`.
- Token: `turn_time`.
- Live TTL: 3 seconds. Completed values: no TTL.
- State version: 1. State files are keyed by the SHA-256 of `HERDR_SOCKET_PATH`.
- The daemon checks `plugin.list` and exits when the plugin is disabled,
  removed, or installed from a different root.
- `ensure` compares the running daemon identity with its own executable. It
  restarts a stale daemon after an update, relink, or local rebuild.
- `stop` clears the token and removes persisted timer state. A later `start`
  begins fresh.
- The sidebar row is manual. Do not add a config-writing action back without
  validating the candidate with `herdr config check` first.

## Verification

```sh
herdr api schema --json
herdr config check
herdr plugin list
herdr plugin action list --plugin cmalec.agent-time
herdr plugin log list --plugin cmalec.agent-time
make check
make test-race
make lint
make build
```
