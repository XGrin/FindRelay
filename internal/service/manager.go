package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Service commands use these errors to describe the current state of the service before or while operating on
// launchd.
var (
	// ErrServiceNotInstalled means the LaunchAgent plist does not exist.
	ErrServiceNotInstalled = errors.New("LaunchAgent is not installed")
	// ErrServiceNotLoaded means the service is not loaded in the current GUI domain.
	ErrServiceNotLoaded = errors.New("LaunchAgent is not loaded in the GUI domain")
	// ErrServiceStillLoaded means launchd still lists the service within the bounded wait after bootout.
	ErrServiceStillLoaded = errors.New("launchd still lists the service after bootout")
	// ErrForeignAgentPlist means the file at the plist path belongs to another service.
	ErrForeignAgentPlist = errors.New("the plist at the LaunchAgent path belongs to another service")
	// ErrLoginLaunchDisabled means the disabled override table has turned off login launch for the service, so
	// launchd refuses to load it again.
	ErrLoginLaunchDisabled = errors.New("login launch is disabled, so launchd would refuse to load the service again")
)

const (
	// DefaultExitTimeout bounds how long to wait for launchd to remove the service after bootout. launchd force-kills
	// the process when its own exit timeout expires; this value leaves enough margin over that.
	DefaultExitTimeout = 30 * time.Second
	defaultPollPeriod  = 50 * time.Millisecond
)

// StepError means a multi-step service command failed at step Failed: Completed lists the steps that succeeded before
// it, and Hint describes the state at that point and the next step.
type StepError struct {
	Command   string
	Completed []string
	Failed    string
	Err       error
	Hint      string
}

func (e *StepError) Error() string { return fmt.Sprintf("%s failed: %v", e.Failed, e.Err) }

func (e *StepError) Unwrap() error { return e.Err }

// steps writes the completed steps of a service command to w line by line, and on error returns them together with
// the error.
type steps struct {
	w         io.Writer
	command   string
	completed []string
}

func (s *steps) say(format string, a ...any) {
	fmt.Fprintf(s.w, "findrelay service %s: %s\n", s.command, fmt.Sprintf(format, a...))
}

// done records and prints one completed step.
func (s *steps) done(step, format string, a ...any) {
	s.completed = append(s.completed, step)
	s.say(format, a...)
}

// fail returns the failed step together with the steps completed before it.
func (s *steps) fail(step string, err error, hint string) error {
	return &StepError{Command: s.command, Completed: s.completed, Failed: step, Err: err, Hint: hint}
}

// GUIDomain returns the launchctl name of the user's GUI login domain.
func GUIDomain(uid int) string { return "gui/" + strconv.Itoa(uid) }

// Manager runs the lifecycle commands of the FindRelay LaunchAgent in the user's GUI domain; the caller
// injects every system interaction. Control flow depends only on launchctl exit codes and on this tool's own plist
// and locks; the text of `launchctl print` is only displayed by status.
type Manager struct {
	Label     string          // service label, i.e. the plist Label
	Domain    string          // launchctl domain, e.g. gui/501
	PlistPath string          // LaunchAgent plist
	RunDir    string          // run directory that holds the service operations lock
	Session   SessionProvider // security session of the current process
	Launchctl LaunchctlRunner

	// ExitTimeout bounds how long to wait for launchd to remove the service after bootout, and PollInterval is the
	// polling interval while waiting.
	ExitTimeout  time.Duration
	PollInterval time.Duration
}

// NewManager builds a Manager with the default wait parameters.
func NewManager(domain, plistPath, runDir string, session SessionProvider, launchctl LaunchctlRunner) *Manager {
	return &Manager{
		Label:        AgentLabel,
		Domain:       domain,
		PlistPath:    plistPath,
		RunDir:       runDir,
		Session:      session,
		Launchctl:    launchctl,
		ExitTimeout:  DefaultExitTimeout,
		PollInterval: defaultPollPeriod,
	}
}

// run executes one service command serially: it first confirms that the current process is in a GUI login session,
// then holds the service operations lock until the command ends.
func (m *Manager) run(command string, w io.Writer, fn func(s *steps, sess SessionInfo) error) error {
	sess, err := guiSessionInfo(m.Session)
	if err != nil {
		return fmt.Errorf("service commands need a GUI login session: %w", err)
	}
	opsLock, err := AcquireOperationsLock(m.RunDir)
	if err != nil {
		return err
	}
	defer func() { _ = opsLock.Release() }()
	return fn(&steps{w: w, command: command}, sess)
}

func (m *Manager) target() string { return m.Domain + "/" + m.Label }

// requireInstalled returns the installed definition; when nothing is installed it asks the user to install first, and
// it refuses a plist path that holds another service's definition.
func (m *Manager) requireInstalled() (*AgentDefinition, error) {
	def, err := ReadAgentDefinition(m.PlistPath)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, fmt.Errorf("%w; run `findrelay service install --app <path>`", ErrServiceNotInstalled)
	}
	if def.Label != m.Label {
		return nil, fmt.Errorf("%w: it records the label %q; run `findrelay service install` to rewrite it", ErrForeignAgentPlist, def.Label)
	}
	return def, nil
}

// loaded uses the exit code of `launchctl print` to decide whether the service is in the current domain.
func (m *Manager) loaded() (bool, error) {
	_, loaded, err := m.printService()
	return loaded, err
}

// printService returns the text of `launchctl print` and whether the service is loaded; the text is empty when the
// domain has no such service.
func (m *Manager) printService() (out string, loaded bool, err error) {
	out, err = m.Launchctl("print", m.target())
	switch {
	case err == nil:
		return out, true, nil
	case launchctlExitCode(err) == LaunchctlExitNotFound:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("query service state: %w", err)
	}
}

// loginLaunchAllowed reads launchd's disabled override table: its state persists across reboots and decides whether
// the service starts at login.
func (m *Manager) loginLaunchAllowed() (bool, error) {
	out, err := m.Launchctl("print-disabled", m.Domain)
	if err != nil {
		return false, fmt.Errorf("query login launch state: %w", err)
	}
	return parseDisabledTable(out, m.Label)
}

// parseDisabledTable reads from the `launchctl print-disabled` text whether the service is disabled. When the table
// has no entry for the service, it counts as allowed, following launchd's semantics; when there is an entry with an
// unrecognized value, it returns an error instead of guessing.
func parseDisabledTable(out, label string) (bool, error) {
	marker := "\"" + label + "\" =>"
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		switch state := strings.TrimSpace(line[i+len(marker):]); state {
		case "disabled":
			return false, nil
		case "enabled":
			return true, nil
		default:
			return false, fmt.Errorf("unsupported state %q for %s in launchctl print-disabled output", state, label)
		}
	}
	// Entries are matched by the full quoted label, so other services whose names contain this service's label (such
	// as io.findrelay.agent.helper) do not count.
	if strings.Contains(out, "\""+label+"\"") {
		return false, fmt.Errorf("unrecognized launchctl print-disabled output for %s", label)
	}
	return true, nil
}

// Install writes ProgramArguments into the LaunchAgent (install). The first install marks the service as disabled
// first, so neither the install nor later logins start it; a repeated install only overwrites the plist with the new
// arguments and keeps the current started or stopped state. This command does not start the service.
func (m *Manager) Install(programArguments []string, w io.Writer) error {
	return m.run("install", w, func(s *steps, _ SessionInfo) error {
		def, err := ReadAgentDefinition(m.PlistPath)
		if err != nil {
			return err
		}
		if def == nil {
			if _, err := m.Launchctl("disable", m.target()); err != nil {
				return s.fail("disable", err, "No plist was written, so nothing is installed.")
			}
			s.done("disable", "login launch disabled.")
		}
		writeHint := "The login launch setting was left unchanged and the service was not started."
		if def == nil {
			// On a first install, disable has already taken effect by the time this point is reached.
			writeHint = "Login launch is disabled and no plist was written, so nothing is installed."
		}
		if err := WriteAgentPlist(m.PlistPath, m.Label, programArguments); err != nil {
			return s.fail("write plist", err, writeHint)
		}
		s.done("write plist", "LaunchAgent written with arguments: %s.", strings.Join(programArguments[1:], " "))
		s.say("Run `findrelay service start` to start it now and allow login launch. An instance that is already running keeps its previous arguments until you run `findrelay service restart`.")
		return nil
	})
}

// loginLaunchOnHint describes the state when a later step fails after start has already allowed login launch.
const loginLaunchOnHint = "Login launch is allowed, so the service will be started at the next login. Fix the problem and rerun `findrelay service start`, or run `findrelay service stop` to disable login launch."

// Start allows login launch and starts the service in the current GUI session (start). validate checks the installed
// definition before launchd is touched, and the error it returns is passed to the caller unchanged. When the service
// is not loaded, it bootstraps; when it is loaded and the session lock is already held, it starts no process and
// leaves the running instance as it is; when it is loaded and the session lock is free, it unloads first and then
// bootstraps, because launchd reads the plist only at bootstrap.
func (m *Manager) Start(validate func(def *AgentDefinition) error, w io.Writer) error {
	return m.run("start", w, func(s *steps, sess SessionInfo) error {
		def, err := m.requireInstalled()
		if err != nil {
			return err
		}
		if err := validate(def); err != nil {
			return err
		}
		if _, err := m.Launchctl("enable", m.target()); err != nil {
			return s.fail("enable", err, "Nothing else was changed.")
		}
		s.done("enable", "login launch allowed.")
		loaded, err := m.loaded()
		if err != nil {
			return s.fail("inspect", err, loginLaunchOnHint)
		}
		if loaded {
			active, err := ProbeInstanceRunning(m.RunDir, sess.ID)
			if err != nil {
				return s.fail("inspect", fmt.Errorf("probe instance lock: %w", err), loginLaunchOnHint)
			}
			if active {
				// The holder is either the service's own instance or a foreground `findrelay run`. kickstart has no
				// effect on the former, and for the latter it would only launch a process that exits at once because
				// of a single-instance conflict (exit code 6), so nothing is started here; status explains who the
				// holder is.
				s.say("The session lock in GUI session %d is already held, so nothing new was started; the running instance was kept.", sess.ID)
				s.say(statusHint)
				return nil
			}
			if err := m.unload(s); err != nil {
				return err
			}
		}
		return m.bootstrap(s, loginLaunchOnHint)
	})
}

// statusHint reminds the user to verify the start result: launchd accepting the start request does not mean that the
// agent process is running or that the event tap is ready.
const statusHint = "Run `findrelay service status` to check whether the agent process runs and the redirection takes effect."

// bootstrap loads the current plist into the GUI domain; RunAtLoad makes launchd launch the agent process right away.
// A submitted start does not mean that the event tap is ready.
func (m *Manager) bootstrap(s *steps, failureHint string) error {
	if _, err := m.Launchctl("bootstrap", m.Domain, m.PlistPath); err != nil {
		return s.fail("bootstrap", err, failureHint)
	}
	s.done("bootstrap", "service loaded into %s; the start request was submitted.", m.Domain)
	s.say(statusHint)
	return nil
}

// unload removes the service from the GUI domain and waits for the old instance to exit. launchctl bootout returns
// immediately, the service leaves the domain only when the process ends, and bootstrap fails in the meantime, so
// unload polls until launchctl no longer lists the service.
func (m *Manager) unload(s *steps) error {
	if _, err := m.Launchctl("bootout", m.target()); err != nil {
		return s.fail("bootout", err, "The service was left as it was.")
	}
	deadline := time.Now().Add(m.ExitTimeout)
	for {
		loaded, err := m.loaded()
		if err != nil {
			return s.fail("wait for exit", err, "bootout was requested; the service may still be stopping.")
		}
		if !loaded {
			break
		}
		if time.Now().After(deadline) {
			return s.fail("wait for exit", ErrServiceStillLoaded, "launchd was asked to remove the service but still lists it; nothing new was started.")
		}
		time.Sleep(m.PollInterval)
	}
	s.done("bootout", "service unloaded from %s; the previous instance has exited.", m.Domain)
	return nil
}

// Restart restarts a loaded service (restart): bootout, wait for the old instance to exit, then bootstrap again,
// without changing the login launch setting. validate runs before launchd is changed, as in start. When the service
// is not loaded or not installed, it returns the matching error and the state stays unchanged. When login launch is
// off, launchd refuses to load the service again, so Restart returns before unloading and points to start, leaving
// the running instance unaffected.
func (m *Manager) Restart(validate func(def *AgentDefinition) error, w io.Writer) error {
	return m.run("restart", w, func(s *steps, _ SessionInfo) error {
		def, err := m.requireInstalled()
		if err != nil {
			return err
		}
		if err := validate(def); err != nil {
			return err
		}
		loaded, err := m.loaded()
		if err != nil {
			return s.fail("inspect", err, "Nothing was changed.")
		}
		if !loaded {
			return fmt.Errorf("%w; run `findrelay service start`", ErrServiceNotLoaded)
		}
		loginAllowed, err := m.loginLaunchAllowed()
		if err != nil {
			return s.fail("inspect", err, "Nothing was changed.")
		}
		if !loginAllowed {
			return fmt.Errorf("%w; run `findrelay service start` to reload it and allow login launch, or `findrelay service stop` to stop it", ErrLoginLaunchDisabled)
		}
		if err := m.unload(s); err != nil {
			return err
		}
		return m.bootstrap(s, "The service is unloaded and its login launch setting is unchanged. Login launch must be allowed to load the service: run `findrelay service start`, or rerun `findrelay service restart` if it already is.")
	})
}

// Stop stops the service and turns off login launch, keeping the plist (stop). A stopped service stays stopped.
func (m *Manager) Stop(w io.Writer) error {
	return m.run("stop", w, func(s *steps, _ SessionInfo) error {
		return m.stop(s)
	})
}

// stop is the stop step shared by stop and uninstall: it first runs disable to turn off login launch, then unloads
// the running service.
func (m *Manager) stop(s *steps) error {
	if _, err := m.Launchctl("disable", m.target()); err != nil {
		return s.fail("disable", err, "A running instance was left running.")
	}
	s.done("disable", "login launch disabled.")
	loaded, err := m.loaded()
	if err != nil {
		return s.fail("inspect", err, "A running instance may still be running.")
	}
	if !loaded {
		s.say("The service is not loaded, so nothing is running.")
		return nil
	}
	return m.unload(s)
}

// Uninstall stops the service, then removes this tool's LaunchAgent plist (uninstall). The executable is left for the
// user to manage, and a repeated uninstall returns safely.
func (m *Manager) Uninstall(w io.Writer) error {
	return m.run("uninstall", w, func(s *steps, _ SessionInfo) error {
		def, err := ReadAgentDefinition(m.PlistPath)
		if err != nil {
			return err
		}
		if def == nil {
			s.say("The LaunchAgent is not installed; nothing to remove.")
			return nil
		}
		if def.Label != m.Label {
			return fmt.Errorf("%w: it records the label %q", ErrForeignAgentPlist, def.Label)
		}
		if err := m.stop(s); err != nil {
			return err
		}
		if err := os.Remove(m.PlistPath); err != nil {
			return s.fail("remove plist", err, "The service is stopped and login launch is disabled; only the plist is left behind.")
		}
		s.done("remove plist", "LaunchAgent removed. The executable is left for you to manage.")
		return nil
	})
}

// agentLogHint points to the diagnostics that the background instance writes to the system unified log.
const agentLogHint = "agent log: log show --predicate 'subsystem == \"%s\"' --last 10m"

// Status reports the installed file, login launch and the state in the launchd domain, and describes the process
// running and the feature being effective separately (status). The feature counts as effective when launchd reports a
// live service process and the session's single-instance lock is held: the agent acquires this lock before creating
// the event tap, and exits immediately and releases it when authorization or tap creation fails. Degraded states are
// visible only in the agent log.
func (m *Manager) Status(w io.Writer) error {
	return m.run("status", w, func(_ *steps, sess SessionInfo) error {
		def, err := ReadAgentDefinition(m.PlistPath)
		if err != nil {
			return err
		}
		loginAllowed, err := m.loginLaunchAllowed()
		if err != nil {
			return err
		}
		out, loaded, err := m.printService()
		if err != nil {
			return err
		}
		active, err := ProbeInstanceRunning(m.RunDir, sess.ID)
		if err != nil {
			return fmt.Errorf("probe instance lock: %w", err)
		}
		m.writeStatus(w, def, loginAllowed, out, loaded, active, sess.ID)
		return nil
	})
}

func (m *Manager) writeStatus(w io.Writer, def *AgentDefinition, loginAllowed bool, printOut string, loaded, active bool, sessionID uint32) {
	field := func(label, format string, a ...any) {
		fmt.Fprintf(w, "  %-13s %s\n", label+":", fmt.Sprintf(format, a...))
	}
	note := func(format string, a ...any) {
		fmt.Fprintf(w, "        %s\n", fmt.Sprintf(format, a...))
	}
	fmt.Fprintln(w, "findrelay service status")
	if def == nil {
		field("Plist", "not installed")
	} else {
		field("Plist", "installed")
		field("Arguments", "%s", strings.Join(def.ProgramArguments, " "))
	}
	launch := "disabled"
	if loginAllowed {
		launch = "allowed"
	}
	field("Login launch", "%s", launch)

	const foreignInstance = "The session lock is held by an instance the service did not start, for example `findrelay run` in a terminal."
	process, function := "none (the service is not loaded)", "not effective"
	var notes []string
	if !loaded {
		field("launchd", "not loaded in %s", m.Domain)
		switch {
		case def == nil:
			notes = append(notes, "Run `findrelay service install --app <path>` to install the LaunchAgent.")
		case loginAllowed:
			notes = append(notes, "The service starts at the next login. Run `findrelay service start` to start it now.")
		default:
			notes = append(notes, "Run `findrelay service start` to start it and allow login launch.")
		}
		if active {
			notes = append(notes, foreignInstance+" It keeps the service from starting.")
		}
	} else {
		field("launchd", "loaded in %s", m.Domain)
		proc, ok := parseLaunchdPrint(printOut)
		if !ok {
			process, function = "unknown", "unknown"
			notes = append(notes, "The process state could not be read from `launchctl print`. Its output:")
			for _, line := range strings.Split(strings.TrimRight(printOut, "\n"), "\n") {
				notes = append(notes, "    "+line)
			}
		} else {
			switch {
			case proc.pid > 0 && active:
				process, function = fmt.Sprintf("running (pid %d)", proc.pid), "effective"
			case proc.pid > 0:
				process = fmt.Sprintf("running (pid %d)", proc.pid)
				notes = append(notes, "The process does not hold the session lock: it is starting or stopping. Run status again; if this persists, the process is stuck.")
			default:
				process = "not running" + proc.exitDescription()
				notes = append(notes, proc.exitHints()...)
				if active {
					notes = append(notes, foreignInstance+" Stop it before starting the service.")
				}
			}
		}
	}
	if active {
		field("Instance", "active in GUI session %d", sessionID)
	} else {
		field("Instance", "none active in GUI session %d", sessionID)
	}
	field("Process", "%s", process)
	field("Function", "%s", function)
	for _, n := range notes {
		note("%s", n)
	}
	note(agentLogHint, AgentLabel)
}

// launchdProcess is the service process information that status reads from the `launchctl print` text. launchctl(1)
// states explicitly that this output is not an API, so only a few fields are taken here for people to read; when none
// of them is recognized, the state is treated as unknown, without affecting other commands.
type launchdProcess struct {
	pid      int    // 0 means no live service process
	exitCode *int   // exit code of the most recent exit that ended with an exit code
	signal   string // signal description launchd recorded for the most recent termination by a signal
}

// exitDescription describes the most recent exit, for example " (exit code 4)"; it is empty when there is no exit
// record.
func (p launchdProcess) exitDescription() string {
	var parts []string
	if p.exitCode != nil {
		parts = append(parts, fmt.Sprintf("exit code %d", *p.exitCode))
	}
	if p.signal != "" {
		parts = append(parts, "signal "+p.signal)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// exitHints turns the most recent exit into next-step hints; when there is no exit record, it explains that the
// service has not run since it was loaded.
func (p launchdProcess) exitHints() []string {
	var hints []string
	if p.exitCode != nil {
		hints = append(hints, exitCodeHint(*p.exitCode))
	}
	if p.signal != "" {
		hints = append(hints, "The agent was terminated by signal "+p.signal+". Run `findrelay service start` to start it again.")
	}
	if len(hints) == 0 {
		hints = append(hints, "The service is loaded but has not run since it was loaded. Run `findrelay service start`.")
	}
	return hints
}

// exitCodeHint turns an agent exit code into a next-step hint.
func exitCodeHint(code int) string {
	switch code {
	case ExitUsage:
		return fmt.Sprintf("The agent rejected the arguments recorded in the LaunchAgent (exit code %d). Reinstall with `findrelay service install`.", code)
	case ExitTarget:
		return fmt.Sprintf("The target application is missing or invalid (exit code %d). Restore it, or reinstall with a valid --app.", code)
	case ExitPermission:
		return fmt.Sprintf("Accessibility permission is not granted to the findrelay executable that launchd starts (exit code %d). Add the executable itself, not your terminal, in System Settings > Privacy & Security > Accessibility, then run `findrelay service start`.", code)
	case ExitRuntime:
		return fmt.Sprintf("Another instance already held the session lock, or the agent hit a runtime failure (exit code %d). Stop any foreground `findrelay run`, then run `findrelay service start`.", code)
	case ExitOK:
		return fmt.Sprintf("The agent exited normally (exit code %d). Run `findrelay service start` to start it again.", code)
	default:
		return fmt.Sprintf("The agent exited with code %d. Details are in the agent log.", code)
	}
}

// parseLaunchdPrint reads only the fields indented one level in `launchctl print`: state, runs, pid and the most
// recent exit record. It returns false when none of them is recognized.
func parseLaunchdPrint(out string) (launchdProcess, bool) {
	var (
		p          launchdProcess
		understood bool
	)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimLeft(line, "\t"), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state", "runs":
			understood = true
		case "pid":
			if pid, err := strconv.Atoi(value); err == nil {
				p.pid, understood = pid, true
			}
		case "last exit code":
			if code, err := strconv.Atoi(value); err == nil {
				p.exitCode, understood = &code, true
			}
		case "last terminating signal":
			p.signal, understood = value, true
		}
	}
	return p, understood
}
