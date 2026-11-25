package diagnostics

// LogLevel is the runtime log level.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Valid reports whether the log level is in the supported set.
func (l LogLevel) Valid() bool {
	switch l {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	default:
		return false
	}
}
