// Command herdr-agent-time shows per-agent turn runtimes in the Herdr sidebar.
//
// # Architecture
//
// The command runs a detached daemon that polls the Herdr Unix socket once per
// second with [Client.ListAgents]. [Tracker.ObserveAll] converts each snapshot
// into [Decision] values, and the daemon writes them with [Client.Report] as
// the turn_time metadata token. Completed turns are persisted by
// [Tracker.Save] and replayed after a Herdr restart.
//
// The daemon watches Herdr's plugin registry with [Client.ListPlugins]. It
// exits when the plugin is disabled, removed, or installed from a different
// root. [ensureDaemon] records the running executable identity and replaces a
// daemon after an update or rebuild.
//
// # Commands
//
// The binary accepts four subcommands: ensure (also start), daemon, stop, and
// status. The Herdr plugin manifest invokes ensure at startup and exposes
// start, stop, and status as global actions. Stop clears the displayed token
// and removes persisted timer state; a later start begins fresh.
//
// # State
//
// Timer state, the daemon lock, PID, identity, and logs live in
// HERDR_PLUGIN_STATE_DIR. Filenames include a hash of HERDR_SOCKET_PATH so
// separate Herdr sessions do not share state. The sidebar row is documented in
// README.md; the plugin does not edit Herdr's config file.
package main
