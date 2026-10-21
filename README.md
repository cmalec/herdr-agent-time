# herdr-agent-time

A [Herdr](https://herdr.dev) plugin that shows each agent's current turn
runtime in the sidebar. While the agent works, the value ticks every second.
When the turn ends, the final duration stays visible until the next turn starts
(or until the agent exits).

```
omp · 0:07      # working
omp · 1:42      # done, frozen at the time the turn took
```

The timer starts on the first `working` observation, keeps counting while the
agent is `blocked` waiting for input, and freezes on `done`, `idle`, or
`unknown`. An interrupted turn keeps its last completed value. The daemon
persists per-pane state under Herdr's plugin state directory, so a daemon or
Herdr server restart resumes a turn when `state_change_seq` is unchanged and
replays a frozen value when the token was lost.

## Install

From GitHub:

```sh
herdr plugin install cmalec/herdr-agent-time
```

For local development:

```sh
go build -trimpath -o bin/herdr-agent-time .
herdr plugin link "$PWD"
herdr plugin action invoke cmalec.agent-time.start
```

Herdr runs the build command during a GitHub install, so Go must be on `PATH`.
Linking a local checkout does not run build commands; build it yourself first.

## Sidebar row

Add the plugin token to the expanded agent rows. This keeps the default first
row and puts the timer next to the harness name on the second row:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", "workspace", "tab"],
  ["agent", "$turn_time"],
]
```

The token disappears when no turn is active. Values use `m:ss`, or `h:mm:ss`
after one hour.

## Lifecycle

The plugin manifest starts a detached timer daemon from Herdr's startup hook.
Actions are available under the `cmalec.agent-time` plugin id:

```sh
herdr plugin action invoke cmalec.agent-time.start
herdr plugin action invoke cmalec.agent-time.stop
herdr plugin action invoke cmalec.agent-time.status
```

The daemon talks to Herdr over its Unix socket. It reads `agent.list` once per
second and writes one `$turn_time` pane token per active agent. A running value
has a three-second TTL, so a stopped daemon leaves no stale live timer. A
completed value has no TTL and is replayed after reconnecting to a restarted
Herdr server.

State and logs live under Herdr's `HERDR_PLUGIN_STATE_DIR` (for example
`~/.local/state/herdr/plugins/cmalec.agent-time/`). State files are keyed by
the socket path, so separate Herdr sessions do not share pane state.

## Limitations

- The timer starts when the daemon first observes a working agent. A turn that
  began before the plugin started is timed from that first observation.
- The plugin relies on Herdr's semantic `working` / `blocked` / `done` / `idle`
  state. Panes without a detected or reported agent are ignored.
- macOS and Linux only; the manifest omits Windows.

## Test

```sh
go test ./...
```
