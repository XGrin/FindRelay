package service

// Exit codes of the agent and the CLI. service status uses them to explain
// why the LaunchAgent process exited.
const (
	ExitOK         = 0 // completed normally, or both redirect entries are disabled
	ExitUsage      = 2 // CLI argument or subcommand usage error
	ExitTarget     = 3 // no target application given, or app bundle validation failed
	ExitPermission = 4 // Accessibility or Input Monitoring permission unavailable
	ExitService    = 5 // service management failure
	ExitRuntime    = 6 // transient runtime error or instance conflict in the same GUI session
)
