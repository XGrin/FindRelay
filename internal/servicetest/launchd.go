// Package servicetest provides an in-memory launchd stand-in that unit tests inject as service.LaunchctlRunner,
// without touching the real launchctl.
package servicetest

import (
	"fmt"
	"strings"
	"sync"

	"github.com/XGrin/findrelay/internal/service"
)

// Launchd simulates the launchctl behavior this tool uses, with values measured on real macOS machines:
//   - disable and enable rewrite the disabled override table, which persists across reboots; a disabled service
//     cannot be bootstrapped.
//   - print returns 0 for a loaded service and 113 when the domain has no such service.
//   - bootout returns immediately, and the service leaves the domain only when its process exits (see UnloadDelay,
//     NeverUnload).
//
// The exported fields are the current launchd state; tests can set initial values and read results directly.
type Launchd struct {
	// Service is the launchctl service target, such as gui/501/io.findrelay.agent.
	Service string
	// Loaded means the service is in the domain; Running and PID describe whether the service process is alive.
	Loaded  bool
	Running bool
	PID     int
	// LastExit and LastSignal are the most recent exit record in the print output; when both are empty, the output is
	// "(never exited)".
	LastExit   string
	LastSignal string
	// Disabled is the entry in the disabled override table: true means disabled, false means enabled, and nil means
	// there is no entry.
	Disabled *bool
	// UnloadDelay is how many times print still reports the service as loaded after bootout; when NeverUnload is
	// true, the service stays in the domain.
	UnloadDelay int
	NeverUnload bool
	// PrintText replaces the print output for a loaded service, to simulate an unrecognized format; when it is empty,
	// the output follows the measured format.
	PrintText string
	// Fail maps subcommands to injected failures; a nil value means the subcommand succeeds as usual.
	Fail map[string]error

	mu       sync.Mutex
	calls    []string
	nextPID  int
	unloadIn int
}

// NewLaunchd returns a launchd stand-in that manages the given service, starting unloaded and with no override entry.
func NewLaunchd(serviceTarget string) *Launchd {
	return &Launchd{Service: serviceTarget, nextPID: 4242}
}

// Calls returns the recorded launchctl calls in order, with the arguments of each call joined by spaces.
func (l *Launchd) Calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// ResetCalls clears the recorded launchctl calls.
func (l *Launchd) ResetCalls() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

// Launchctl acts as service.LaunchctlRunner.
func (l *Launchd) Launchctl(args ...string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, strings.Join(args, " "))
	if err, ok := l.Fail[args[0]]; ok && err != nil {
		output := "injected failure for " + args[0] + "\n"
		return output, &service.LaunchctlError{Args: args, Code: 5, Output: output, Err: err}
	}
	switch args[0] {
	case "disable", "enable", "bootout":
		if args[1] != l.Service {
			return l.notFound(args)
		}
	}
	switch args[0] {
	case "disable", "enable":
		disabled := args[0] == "disable"
		l.Disabled = &disabled
		return "", nil
	case "print":
		return l.print(args)
	case "print-disabled":
		return l.printDisabled(), nil
	case "bootstrap":
		return l.bootstrap(args)
	case "bootout":
		return l.bootout(args)
	default:
		return "", fmt.Errorf("launchd fake: unsupported subcommand %q", args[0])
	}
}

// failure returns a failure the way the real launchctl does: both the exit code and the output go into a
// LaunchctlError.
func failure(args []string, code int, output string) (string, error) {
	return output, &service.LaunchctlError{Args: args, Code: code, Output: output, Err: fmt.Errorf("exit status %d", code)}
}

func (l *Launchd) notFound(args []string) (string, error) {
	return failure(args, service.LaunchctlExitNotFound, "Bad request.\nCould not find service \""+l.Service+"\" in domain for user gui: 501\n")
}

func (l *Launchd) print(args []string) (string, error) {
	if !l.Loaded {
		return l.notFound(args)
	}
	out := l.PrintText
	if out == "" {
		out = l.serviceDescription()
	}
	if l.unloadIn > 0 {
		l.unloadIn--
		if l.unloadIn == 0 {
			l.Loaded, l.Running, l.PID = false, false, 0
		}
	}
	return out, nil
}

// serviceDescription generates the service description in the format of `launchctl print` on a real machine.
func (l *Launchd) serviceDescription() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s = {\n\tactive count = ", l.Service)
	if l.Running {
		b.WriteString("1")
	} else {
		b.WriteString("0")
	}
	b.WriteString("\n\ttype = LaunchAgent\n\tstate = ")
	if l.Running {
		b.WriteString("running")
	} else {
		b.WriteString("not running")
	}
	b.WriteString("\n\n\tdefault environment = {\n\t\tPATH => /usr/bin:/bin\n\t}\n\n\truns = 1\n")
	if l.Running {
		fmt.Fprintf(&b, "\tpid = %d\n", l.PID)
	}
	b.WriteString("\n\tlast exit code = ")
	if l.LastExit != "" {
		b.WriteString(l.LastExit)
	} else {
		b.WriteString("(never exited)")
	}
	b.WriteString("\n")
	if l.LastSignal != "" {
		b.WriteString("\tlast terminating signal = ")
		b.WriteString(l.LastSignal)
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// printDisabled generates the disabled override table in the format of `launchctl print-disabled` on a real machine,
// plus an entry for an unrelated service.
func (l *Launchd) printDisabled() string {
	label := l.Service[strings.LastIndex(l.Service, "/")+1:]
	var b strings.Builder
	b.WriteString("\n\tdisabled services = {\n\t\t\"com.example.other\" => enabled\n")
	if l.Disabled != nil {
		b.WriteString("\t\t\"")
		b.WriteString(label)
		b.WriteString("\" => ")
		if *l.Disabled {
			b.WriteString("disabled")
		} else {
			b.WriteString("enabled")
		}
		b.WriteString("\n")
	}
	b.WriteString("\t}\n\n\tdisabled sessions = {\n\n\t}\n")
	return b.String()
}

func (l *Launchd) bootstrap(args []string) (string, error) {
	// launchctl bootstrap <domain> <plist>
	if (l.Disabled != nil && *l.Disabled) || l.Loaded {
		return failure(args, 5, "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.\n")
	}
	l.Loaded, l.Running = true, true
	l.PID, l.nextPID = l.nextPID, l.nextPID+1
	l.LastExit, l.LastSignal = "", ""
	return "", nil
}

func (l *Launchd) bootout(args []string) (string, error) {
	if !l.Loaded {
		return failure(args, 3, "Boot-out failed: 3: No such process\n")
	}
	switch {
	case l.NeverUnload:
	case l.UnloadDelay > 0:
		if l.unloadIn == 0 {
			l.unloadIn = l.UnloadDelay
		}
	default:
		l.Loaded, l.Running, l.PID = false, false, 0
	}
	return "", nil
}
