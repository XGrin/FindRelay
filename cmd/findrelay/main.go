// Command findrelay forwards selected Dock and native desktop gestures to the file manager the user chose.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
	"github.com/XGrin/findrelay/internal/relay"
	"github.com/XGrin/findrelay/internal/service"
)

func init() {
	// The main goroutine is pinned to the OS main thread, which serves AppKit / NSWorkspace main-queue calls.
	runtime.LockOSThread()
}

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

// Exit codes shared with the service package; see internal/service/exitcode.go.
const (
	exitOK         = service.ExitOK
	exitUsage      = service.ExitUsage
	exitTarget     = service.ExitTarget
	exitPermission = service.ExitPermission
	exitService    = service.ExitService
	exitRuntime    = service.ExitRuntime
)

const usage = `findrelay <command> [flags]

Commands:
  version    Print the version
  doctor     Check the system, permission preflight, target application and installed service
  run        Run the agent in the foreground; flags are the whole configuration, no config file is written (see findrelay run --help)
  service    Manage the login LaunchAgent: install, start, stop, status and uninstall (see findrelay service --help)
`

const runUsage = `Usage:
  findrelay run --app <path> [--desktop] [--dock=false] [--open-command <template>]
                [--dock-intercept=up|down] [--desktop-intercept=up|down] [--debug] [--log-level=debug|info|warn|error]
  findrelay run --observe [--debug]

--app            Absolute path of the target .app bundle; required in active mode, not needed with --observe.
--dock           Redirect Finder clicks in the Dock (on by default; --dock=false turns it off).
--desktop        Redirect folder double-clicks on the native desktop (off by default).
--open-command   Command template that hands the folder to the target app. It runs through /bin/sh, the folder path
                 is passed in an environment variable and must be referenced as "$folder".
With both entries off, findrelay exits without creating an event tap.
`

const doctorUsage = `Usage:
  findrelay doctor [--app <path>]

Without --app, doctor checks the target application recorded in the installed service, if there is one.
`

const serviceUsage = `Usage:
  findrelay service install [run flags]   # write the flags into the LaunchAgent without starting it
  findrelay service start           # start in the current session and allow launch at login
  findrelay service status          # plist, login launch, process state and whether redirection is in effect
  findrelay service restart         # restart the loaded service
  findrelay service stop            # stop and turn off launch at login, keeping the plist
  findrelay service uninstall       # stop and remove the LaunchAgent

install takes the same flags as run, for example --app <path> [--desktop]; the LaunchAgent records the absolute
path of the executable together with these flags.
`

// errHelp means the user asked for the usage text: the usage has been written to stdout, so the process should exit
// successfully.
var errHelp = errors.New("help requested")

// errAppRequired means active mode was run without a target application.
var errAppRequired = errors.New("--app is required for active mode")

type tapHandle interface {
	Stop()
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// runtimeEnv collects every system interaction main needs, and tests replace them with stand-ins; the caller supplies
// every field.
type runtimeEnv struct {
	runDir              func() (string, error)
	agentPlistPath      func() (string, error)
	selfExecutable      func() (string, error)
	finderAppPath       func() string
	inspectBundle       appbundle.BundleInspector
	sessionInfo         service.SessionProvider
	systemInfo          func() diagnostics.SystemVersion
	isAXTrusted         func(prompt bool) bool
	openAppAsync        relay.AppOpener
	openDirAsync        relay.DirectoryOpener
	openDirViaCommand   darwin.CommandDirectoryOpener
	classifyDirectory   relay.DirectoryClassifier
	revalidateDirectory relay.DirectoryRevalidator
	doubleClickInterval func() uint64
	newProbe            func(finderPath string) (relay.HitProbe, func(), error)
	warmupAX            func(probe relay.HitProbe, mode darwin.InspectMode, recorder *diagnostics.Recorder)
	startTap            func(listenOnly bool, cb darwin.TapCallbacks) (tapHandle, error)
	prepareMainLoop     func()
	runMainLoop         func()
	stopMainLoop        func()
	shutdownSignal      func() (<-chan struct{}, func())
	// launchctl operates on the LaunchAgent in the user's GUI domain; serviceMode reports whether this process was
	// started by the LaunchAgent, and serviceLog is the writer such a background instance uses for diagnostics.
	launchctl   service.LaunchctlRunner
	serviceMode func() bool
	serviceLog  func() io.WriteCloser
}

func defaultRuntimeEnv() runtimeEnv {
	return runtimeEnv{
		runDir:              service.DefaultRunDir,
		agentPlistPath:      service.DefaultAgentPlistPath,
		selfExecutable:      os.Executable,
		finderAppPath:       darwin.FinderApplicationPath,
		inspectBundle:       darwin.InspectAppBundle,
		sessionInfo:         darwin.CurrentGUISession,
		systemInfo:          readDarwinSystemVersion,
		isAXTrusted:         darwin.IsProcessTrusted,
		openAppAsync:        darwin.OpenApplicationAsync,
		openDirAsync:        darwin.OpenDirectoryAsync,
		openDirViaCommand:   darwin.OpenDirectoryViaCommand,
		classifyDirectory:   darwin.ClassifyDirectory,
		revalidateDirectory: darwin.RevalidateDirectory,
		doubleClickInterval: darwin.DoubleClickIntervalNS,
		newProbe: func(finderPath string) (relay.HitProbe, func(), error) {
			ax, err := darwin.NewAXInspector(finderPath)
			if err != nil {
				return nil, nil, err
			}
			return ax, ax.Close, nil
		},
		warmupAX: relay.WarmUpAX,
		startTap: func(listenOnly bool, cb darwin.TapCallbacks) (tapHandle, error) {
			return darwin.StartEventTap(listenOnly, cb)
		},
		prepareMainLoop: darwin.PrepareMainLoop,
		runMainLoop:     darwin.RunMainLoop,
		stopMainLoop:    darwin.StopMainLoop,
		shutdownSignal: func() (<-chan struct{}, func()) {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			return ctx.Done(), stop
		},
		launchctl:   service.Launchctl,
		serviceMode: func() bool { return os.Getenv(service.ServiceEnvironmentKey) == service.ServiceEnvironmentValue },
		serviceLog:  darwin.NewServiceLogWriter,
	}
}

func readDarwinSystemVersion() diagnostics.SystemVersion {
	pv, _ := syscall.Sysctl("kern.osproductversion")
	bv, _ := syscall.Sysctl("kern.osversion")
	return diagnostics.SystemVersion{
		ProductVersion: pv,
		BuildVersion:   bv,
		Arch:           runtime.GOARCH,
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithEnv(args, stdout, stderr, defaultRuntimeEnv())
}

func runWithEnv(args []string, stdout, stderr io.Writer, env runtimeEnv) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	switch args[0] {
	case "version":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "findrelay version: unexpected arguments\n\n%s", usage)
			return exitUsage
		}
		fmt.Fprintf(stdout, "findrelay %s\n", version)
		return exitOK
	case "doctor":
		return runDoctorCmd(args[1:], stdout, stderr, env)
	case "run":
		return runAgentCmd(args[1:], stdout, stderr, env)
	case "service":
		return runServiceCmd(args[1:], stdout, stderr, env)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "findrelay: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}

// parseSubcommand parses subcommand arguments: for --help it writes the usage to stdout and returns errHelp; for
// other usage errors (including extra positional arguments) it writes a message to stderr and returns the error
// unchanged.
func parseSubcommand(name string, fs *flag.FlagSet, args []string, usage string, stdout, stderr io.Writer) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return errHelp
		}
		fmt.Fprintf(stderr, "findrelay %s: %v\n\n%s", name, err, usage)
		return err
	}
	if fs.NArg() > 0 {
		err := errors.New("unexpected positional arguments")
		fmt.Fprintf(stderr, "findrelay %s: %v\n\n%s", name, err, usage)
		return err
	}
	return nil
}

// validateApp normalizes and validates the target application bundle path and returns the normalized absolute path.
func (env runtimeEnv) validateApp(appPath string) (string, appbundle.BundleInfo, error) {
	norm, err := appbundle.NormalizeCLIAppPath(appPath)
	if err != nil {
		return "", appbundle.BundleInfo{}, err
	}
	selfExec, _ := env.selfExecutable()
	info, err := appbundle.ValidateApplication(norm, appbundle.ValidateOptions{
		Inspector:      env.inspectBundle,
		SelfExecutable: selfExec,
		FinderAppPath:  env.finderAppPath(),
	})
	return norm, info, err
}

func runDoctorCmd(args []string, stdout, stderr io.Writer, env runtimeEnv) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	appFlag := fs.String("app", "", "Target .app bundle path to validate")
	if err := parseSubcommand("doctor", fs, args, doctorUsage, stdout, stderr); err != nil {
		if errors.Is(err, errHelp) {
			return exitOK
		}
		return exitUsage
	}

	plistPath, err := env.agentPlistPath()
	if err != nil {
		fmt.Fprintf(stderr, "findrelay doctor: %v\n", err)
		return exitRuntime
	}
	agentArgs, err := service.ReadAgentArguments(plistPath)
	if err != nil {
		fmt.Fprintf(stderr, "findrelay doctor: %v\n", err)
		return exitRuntime
	}
	runDir, err := env.runDir()
	if err != nil {
		fmt.Fprintf(stderr, "findrelay doctor: %v\n", err)
		return exitRuntime
	}

	// When no target application is given explicitly, reuse the target application from the installed service
	// arguments.
	appPath := *appFlag
	if appPath == "" && agentArgs != nil {
		if installed, ok := service.FlagValue(agentArgs, "app"); ok {
			appPath = installed
		}
	}

	_, docErr := diagnostics.RunDoctor(stdout, diagnostics.DoctorDeps{
		Version:     version,
		AppPath:     appPath,
		ValidateApp: func(p string) (appbundle.BundleInfo, error) { _, info, err := env.validateApp(p); return info, err },
		AgentPlist:  plistPath,
		AgentArgs:   agentArgs,
		RunDir:      runDir,
		SystemInfo:  env.systemInfo,
		SessionInfo: env.sessionInfo,
		IsAXTrusted: func() bool { return env.isAXTrusted(false) },
		ProbeRunning: func(runDir string, sessionID uint32) (bool, error) {
			return service.ProbeInstanceRunning(runDir, sessionID)
		},
	})
	if docErr != nil {
		if errors.Is(docErr, appbundle.ErrInvalidApp) {
			return exitTarget
		}
		return exitRuntime
	}
	return exitOK
}

// runFlags holds all parsed arguments of `run`; the arguments are the configuration, and there is no other
// configuration source at runtime.
type runFlags struct {
	appPath          string
	dock             bool
	desktop          bool
	openCommand      string
	debug            bool
	observe          bool
	logLevel         diagnostics.LogLevel
	dockIntercept    policy.InterceptMode
	desktopIntercept policy.InterceptMode
}

// parseRunFlags parses and validates the arguments of run; name and usage set the prefix and text of usage-error
// output, and errHelp means the usage has been written to stdout. `service install` uses the same function to parse
// the same arguments as run.
func parseRunFlags(name, usage string, args []string, stdout, stderr io.Writer) (runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	appFlag := fs.String("app", "", "Target .app bundle path (required unless --observe)")
	dockFlag := fs.Bool("dock", true, "Redirect Dock Finder clicks")
	desktopFlag := fs.Bool("desktop", false, "Redirect desktop folder double-clicks")
	openCommandFlag := fs.String("open-command", "", "Shell command template that opens a folder in the target app; must reference \"$folder\"")
	debugFlag := fs.Bool("debug", false, "Enable sanitized debug logging")
	observeFlag := fs.Bool("observe", false, "Passive observation mode (listen-only event tap)")
	logLevelFlag := fs.String("log-level", "warn", "Runtime log level (debug|info|warn|error)")
	interceptFlag := fs.String("dock-intercept", "up", "Dock interception point: up (default) or down")
	desktopInterceptFlag := fs.String("desktop-intercept", "up", "Desktop double-click interception point: up (default) or down")

	if err := parseSubcommand(name, fs, args, usage, stdout, stderr); err != nil {
		return runFlags{}, err
	}

	usageErr := func(err error) (runFlags, error) {
		fmt.Fprintf(stderr, "findrelay %s: %v\n\n%s", name, err, usage)
		return runFlags{}, err
	}

	var rf runFlags
	var err error
	if rf.dockIntercept, err = policy.ParseInterceptMode(*interceptFlag, "dock"); err != nil {
		return usageErr(err)
	}
	if rf.desktopIntercept, err = policy.ParseInterceptMode(*desktopInterceptFlag, "desktop"); err != nil {
		return usageErr(err)
	}
	rf.logLevel = diagnostics.LogLevel(*logLevelFlag)
	if !rf.logLevel.Valid() {
		return usageErr(fmt.Errorf("unsupported log level %q", *logLevelFlag))
	}
	if *openCommandFlag != "" {
		if rf.openCommand, err = darwin.NormalizeOpenCommand(*openCommandFlag); err != nil {
			return usageErr(err)
		}
	}

	rf.appPath, rf.dock, rf.desktop = *appFlag, *dockFlag, *desktopFlag
	rf.debug, rf.observe = *debugFlag, *observeFlag
	if rf.debug {
		rf.logLevel = diagnostics.LogDebug
	}
	return rf, nil
}

// agentArguments returns the canonical run arguments equivalent to the parsed result, for writing into the
// LaunchAgent; parseRunFlags reads them back unchanged. appPath must be a validated absolute path: when the
// LaunchAgent starts, there is no current directory to resolve a relative path against.
func (rf runFlags) agentArguments(appPath string) []string {
	args := []string{"run"}
	if rf.observe {
		args = append(args, "--observe")
	} else {
		args = append(args, "--app", appPath)
	}
	if !rf.dock {
		args = append(args, "--dock=false")
	}
	if rf.desktop {
		args = append(args, "--desktop")
	}
	if rf.openCommand != "" {
		args = append(args, "--open-command", rf.openCommand)
	}
	if rf.debug {
		args = append(args, "--debug")
	} else if rf.logLevel != diagnostics.LogWarn {
		args = append(args, "--log-level", string(rf.logLevel))
	}
	if rf.dockIntercept != policy.InterceptOnUp {
		args = append(args, "--dock-intercept="+rf.dockIntercept.String())
	}
	if rf.desktopIntercept != policy.InterceptOnUp {
		args = append(args, "--desktop-intercept="+rf.desktopIntercept.String())
	}
	return args
}

// target is the target application validated at startup; it is the zero value in observe mode.
type target struct {
	appPath string
	info    appbundle.BundleInfo
}

// resolveTarget validates the target application according to the entry switches: active mode requires a valid --app,
// and observe mode validates it only when one is given.
func resolveTarget(rf runFlags, env runtimeEnv) (target, error) {
	if rf.appPath == "" {
		if rf.observe {
			return target{}, nil
		}
		return target{}, errAppRequired
	}
	appPath, info, err := env.validateApp(rf.appPath)
	if err != nil {
		return target{}, err
	}
	if rf.observe {
		return target{}, nil
	}
	return target{appPath: appPath, info: info}, nil
}

// targetHint turns a target application validation error into a hint and, when the target is missing, includes an
// example command built from name. In serviceMode it gives only the error category: diagnostics are written to the
// system unified log, and the target application path is not recorded.
func targetHint(name string, err error, serviceMode bool) string {
	if errors.Is(err, errAppRequired) {
		return fmt.Sprintf("%v, e.g. findrelay %s --app \"/Applications/<App>.app\"", err, name)
	}
	if serviceMode && errors.Is(err, appbundle.ErrInvalidApp) {
		return "invalid target application; reinstall with `findrelay service install --app <path>`"
	}
	return err.Error()
}

// entriesEnabled reports whether observe mode is on or at least one redirect entry is enabled; when neither is on,
// the agent has nothing to do.
func entriesEnabled(rf runFlags) bool {
	return rf.observe || rf.dock || rf.desktop
}

// directoryOpenerFor decides how a desktop directory is handed to the target application: when an open command is
// given, the directory goes through that command, while the fallback to Finder still uses the standard system method.
func directoryOpenerFor(openCommand, targetApp string, env runtimeEnv) relay.DirectoryOpener {
	if openCommand == "" {
		return env.openDirAsync
	}
	viaCommand, systemOpen := env.openDirViaCommand, env.openDirAsync
	return func(dirPath, appPath string, onComplete func(error)) {
		if appPath == targetApp {
			viaCommand(openCommand, dirPath, onComplete)
			return
		}
		systemOpen(dirPath, appPath, onComplete)
	}
}

// rerunHint gives the retry hint: the user restarts a foreground instance manually, while a LaunchAgent instance does
// not come back by itself after it exits and must be started again with service start.
func rerunHint(serviceMode bool) string {
	if serviceMode {
		return "then run `findrelay service start`"
	}
	return "then rerun"
}

// accessibilityHint says who should be granted Accessibility: the LaunchAgent runs the findrelay executable itself,
// so the grantee is not the terminal.
func accessibilityHint(serviceMode bool) string {
	if serviceMode {
		return "The LaunchAgent runs the findrelay executable itself: add that executable, not your terminal, in System Settings > Privacy & Security > Accessibility, then run `findrelay service start`."
	}
	return "Allow your terminal or findrelay in System Settings > Privacy & Security > Accessibility, then rerun."
}

// instanceConflictHint explains, when a LaunchAgent instance exits because of a single-instance conflict, where the
// conflict comes from and how to resolve it.
func instanceConflictHint(serviceMode bool, err error) string {
	if serviceMode && errors.Is(err, service.ErrInstanceAlreadyRunning) {
		return ". The LaunchAgent instance exits; stop the other instance (Ctrl+C in its terminal, or `findrelay service stop`) before starting the service again."
	}
	return ""
}

// runAgentCmd runs the agent. When the LaunchAgent starts it there is no terminal: diagnostics go to the system
// unified log, and the entry state and exit reason are recorded separately.
func runAgentCmd(args []string, stdout, stderr io.Writer, env runtimeEnv) (exitCode int) {
	rf, err := parseRunFlags("run", runUsage, args, stdout, stderr)
	if err != nil {
		if errors.Is(err, errHelp) {
			return exitOK
		}
		return exitUsage
	}

	var exitReason string
	sink := io.Writer(stderr)
	if env.serviceMode() {
		serviceLog := env.serviceLog()
		sink = serviceLog
		defer func() {
			fmt.Fprintf(sink, "findrelay run: exited with code %d%s\n", exitCode, exitReason)
			_ = serviceLog.Close()
		}()
	}
	safeErr := &syncWriter{w: sink}

	if env.serviceMode() {
		openMode := "system"
		if rf.openCommand != "" {
			openMode = "command"
		}
		fmt.Fprintf(safeErr, "findrelay run: service instance starting (dock=%v desktop=%v observe=%v open=%s log_level=%s)\n",
			rf.dock, rf.desktop, rf.observe, openMode, rf.logLevel)
	}

	tgt, err := resolveTarget(rf, env)
	if err != nil {
		fmt.Fprintf(safeErr, "findrelay run: %s\n", targetHint("run", err, env.serviceMode()))
		return exitTarget
	}
	if !entriesEnabled(rf) {
		fmt.Fprintln(safeErr, "findrelay run: both entries are disabled (--dock=false and no --desktop); exiting without creating event tap")
		return exitOK
	}

	// Before creating the tap, acquire the exclusive single-instance lock of the current GUI session and hold it
	// until the native callbacks have fully finished.
	runDir, err := env.runDir()
	if err != nil {
		fmt.Fprintf(safeErr, "findrelay run: %v\n", err)
		return exitRuntime
	}
	instLock, err := service.AcquireInstanceLock(runDir, env.sessionInfo)
	if err != nil {
		fmt.Fprintf(safeErr, "findrelay run: %v%s\n", err, instanceConflictHint(env.serviceMode(), err))
		return exitRuntime
	}
	defer func() {
		_ = instLock.Release()
	}()

	// Preflight the Accessibility authorization at startup.
	if !env.isAXTrusted(true) {
		fmt.Fprintf(safeErr, "findrelay run: Accessibility permission is not granted. %s\n", accessibilityHint(env.serviceMode()))
		return exitPermission
	}

	finderPath := env.finderAppPath()
	probe, closeProbe, err := env.newProbe(finderPath)
	if err != nil {
		fmt.Fprintf(safeErr, "findrelay run: %v\n", err)
		return exitPermission
	}

	recorder := diagnostics.NewRecorder(safeErr, rf.logLevel, 256)
	defer recorder.Close()

	// Degradation is reported only by a redacted reason code; the recorder emits the detailed log.
	notice := func(reason string) {
		hint := ""
		if reason == relay.ReasonDesktopPermissionDenied {
			hint = " Desktop redirection stays off until the Desktop folder is readable (System Settings > Privacy & Security > Files and Folders); Dock redirection continues."
		}
		fmt.Fprintf(safeErr, "findrelay run: degraded (%s).%s\n", reason, hint)
	}

	var (
		fatalOnce   sync.Once
		fatalReason string
		fatalCh     = make(chan struct{})
	)
	triggerFatal := func(reason string) {
		fatalOnce.Do(func() {
			fatalReason = reason
			close(fatalCh)
		})
	}

	var (
		executor   *relay.Executor
		classifier *relay.Classifier
		snapshots  policy.SnapshotSource
	)
	if !rf.observe {
		completionTimeout := relay.DefaultCompletionTimeout
		if rf.openCommand != "" {
			completionTimeout = relay.DefaultCommandCompletionTimeout
		}
		executor = relay.NewExecutor(relay.ExecutorConfig{
			TargetAppPath:     tgt.appPath,
			FinderAppPath:     finderPath,
			Capacity:          relay.DefaultActionCapacity,
			CompletionTimeout: completionTimeout,
			Opener:            env.openAppAsync,
			DirOpener:         directoryOpenerFor(rf.openCommand, tgt.appPath, env),
			// At runtime, only confirm that the app bundle and the executable still exist; the full identity and path
			// validation was done at startup.
			RevalidateTarget: func(string) error {
				return appbundle.Revalidate(tgt.appPath, tgt.info)
			},
			RevalidateDirectory: env.revalidateDirectory,
			Recorder:            recorder,
			OnTargetInvalid:     notice,
		})
		if rf.desktop {
			// Desktop directory metadata is accessed only after the user explicitly enables it; when reading is
			// denied, only the desktop entry degrades and the Dock entry keeps running.
			classifier = relay.NewClassifier(env.classifyDirectory, recorder, notice)
			snapshots = classifier
		}
	}

	ctrl := relay.NewController(relay.ControllerConfig{
		ObserveOnly:           rf.observe,
		DockEnabled:           rf.dock,
		DockInterceptMode:     rf.dockIntercept,
		DesktopEnabled:        rf.desktop,
		DesktopInterceptMode:  rf.desktopIntercept,
		DoubleClickIntervalNS: env.doubleClickInterval(),
		Snapshots:             snapshots,
		Probe:                 probe,
		Executor:              executor,
		Recorder:              recorder,
		IsTrusted:             func() bool { return env.isAXTrusted(false) },
		OnFatalStop:           triggerFatal,
	})

	// Pay the AX cold-start cost before creating the tap: no tap callbacks exist yet, so warmup does not
	// compete with any query for the message timeout. Any failed step only logs one redacted line.
	env.warmupAX(probe, ctrl.InspectMode(), recorder)

	// shutdown is the only shutdown order: stop the tap first (pass nil when there is no tap to stop), then close the
	// classifier and executor that only the tap thread uses, and stop the main queue last.
	shutdown := func(tap tapHandle) {
		if tap != nil {
			tap.Stop()
		}
		ctrl.CloseProbe(closeProbe)
		if classifier != nil {
			classifier.Close()
		}
		if executor != nil {
			executor.Close()
		}
		env.stopMainLoop()
		darwin.SetSystemResetHandler(nil)
	}

	env.prepareMainLoop()
	darwin.SetSystemResetHandler(ctrl.OnSystemReset)

	if rf.observe {
		fmt.Fprintln(safeErr, "findrelay observing left-mouse events (listen-only). Press Ctrl+C to stop.")
	} else if rf.debug {
		openMode := "system"
		if rf.openCommand != "" {
			openMode = "command"
		}
		fmt.Fprintf(safeErr, "findrelay listening (dock=%v intercept=%s, desktop=%v intercept=%s, open=%s). Press Ctrl+C to stop.\n",
			rf.dock, rf.dockIntercept, rf.desktop, rf.desktopIntercept, openMode)
	}

	tap, err := env.startTap(rf.observe, ctrl)
	if err != nil {
		shutdown(nil)
		fmt.Fprintf(safeErr, "findrelay run: cannot create event tap. Check System Settings > Privacy & Security > Accessibility and Input Monitoring permissions, %s.\n", rerunHint(env.serviceMode()))
		return exitPermission
	}

	sigCh, stopSig := env.shutdownSignal()
	defer stopSig()

	var graceful bool
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-sigCh:
			graceful = true
			// Graceful stop: first let in-flight consumed-press pairs finish within 250ms, then shut down in the
			// standard order. Restore the default signal handling so that a second Ctrl+C ends the wait immediately.
			stopSig()
			ctrl.DrainForShutdown(250 * time.Millisecond)
		case <-fatalCh:
		}
		shutdown(tap)
	}()

	env.runMainLoop()
	<-shutdownDone

	// The tap has stopped and all callbacks have finished, so the metrics are final; once the queue is drained, the
	// summary line becomes the last diagnostic line.
	recorder.Close()
	fmt.Fprintf(safeErr, "findrelay run: %s\n", recorder.LatencySummary())

	if !graceful && fatalReason != "" {
		exitReason = " (" + fatalReason + ")"
		if fatalReason == "permission_revoked" {
			return exitPermission
		}
		return exitRuntime
	}
	return exitOK
}

// runServiceCmd parses and dispatches the `service` subcommands.
func runServiceCmd(args []string, stdout, stderr io.Writer, env runtimeEnv) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "findrelay service: missing subcommand\n\n%s", serviceUsage)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	if sub == "-h" || sub == "--help" || sub == "help" {
		fmt.Fprint(stdout, serviceUsage)
		return exitOK
	}
	if sub == "install" {
		return runServiceInstallCmd(rest, stdout, stderr, env)
	}
	switch sub {
	case "start", "status", "restart", "stop", "uninstall":
		fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if err := parseSubcommand("service "+sub, fs, rest, serviceUsage, stdout, stderr); err != nil {
			if errors.Is(err, errHelp) {
				return exitOK
			}
			return exitUsage
		}
		return runServiceActionCmd(sub, stdout, stderr, env)
	default:
		fmt.Fprintf(stderr, "findrelay service: unknown subcommand %q\n\n%s", sub, serviceUsage)
		return exitUsage
	}
}

// newServiceManager builds the LaunchAgent manager for the current user's GUI domain.
func newServiceManager(env runtimeEnv) (*service.Manager, error) {
	plistPath, err := env.agentPlistPath()
	if err != nil {
		return nil, err
	}
	runDir, err := env.runDir()
	if err != nil {
		return nil, err
	}
	return service.NewManager(service.GUIDomain(os.Getuid()), plistPath, runDir, env.sessionInfo, env.launchctl), nil
}

// runServiceInstallCmd validates the same arguments as run, then writes the absolute path of the executable and these
// arguments into the LaunchAgent; it does not start the service.
func runServiceInstallCmd(args []string, stdout, stderr io.Writer, env runtimeEnv) int {
	rf, err := parseRunFlags("service install", serviceUsage, args, stdout, stderr)
	if err != nil {
		if errors.Is(err, errHelp) {
			return exitOK
		}
		return exitUsage
	}
	tgt, err := resolveTarget(rf, env)
	if err != nil {
		fmt.Fprintf(stderr, "findrelay service install: %s\n", targetHint("service install", err, false))
		return exitTarget
	}
	if !entriesEnabled(rf) {
		fmt.Fprintf(stderr, "findrelay service install: both entries are disabled (--dock=false and no --desktop); the service would exit at once\n\n%s", serviceUsage)
		return exitUsage
	}
	executable, err := env.selfExecutable()
	if err != nil {
		fmt.Fprintf(stderr, "findrelay service install: %v\n", err)
		return exitService
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		fmt.Fprintf(stderr, "findrelay service install: %v\n", err)
		return exitService
	}
	manager, err := newServiceManager(env)
	if err != nil {
		fmt.Fprintf(stderr, "findrelay service install: %v\n", err)
		return exitService
	}
	if err := manager.Install(append([]string{executable}, rf.agentArguments(tgt.appPath)...), stdout); err != nil {
		reportServiceError("install", err, stderr)
		return serviceExitCode(err)
	}
	return exitOK
}

// runServiceActionCmd runs the service commands other than install; start and restart validate the installed
// definition before touching launchd.
func runServiceActionCmd(sub string, stdout, stderr io.Writer, env runtimeEnv) int {
	manager, err := newServiceManager(env)
	if err == nil {
		validate := func(def *service.AgentDefinition) error { return validateAgentDefinition(def, env) }
		switch sub {
		case "start":
			err = manager.Start(validate, stdout)
		case "restart":
			err = manager.Restart(validate, stdout)
		case "status":
			err = manager.Status(stdout)
		case "stop":
			err = manager.Stop(stdout)
		case "uninstall":
			err = manager.Uninstall(stdout)
		}
	}
	if err != nil {
		reportServiceError(sub, err, stderr)
		return serviceExitCode(err)
	}
	return exitOK
}

// validateAgentDefinition validates the installed LaunchAgent definition: ProgramArguments must be, in order, the
// executable as an absolute path, the subcommand run, and the run arguments; the executable must still exist, the
// arguments must parse, the target application must be valid, and at least one entry must be enabled.
func validateAgentDefinition(def *service.AgentDefinition, env runtimeEnv) error {
	programArgs := def.ProgramArguments
	if len(programArgs) < 2 || programArgs[1] != "run" || !filepath.IsAbs(programArgs[0]) {
		return fmt.Errorf("%w: ProgramArguments must be the absolute executable path followed by `run` and its flags", service.ErrAgentPlistUnreadable)
	}
	info, err := os.Stat(programArgs[0])
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("the executable recorded in the LaunchAgent is missing or not executable: %s", programArgs[0])
	}
	rf, err := parseRunFlags("service start", serviceUsage, programArgs[2:], io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("the arguments recorded in the LaunchAgent are invalid: %v", err)
	}
	if _, err := resolveTarget(rf, env); err != nil {
		return fmt.Errorf("the target application recorded in the LaunchAgent is invalid: %w", err)
	}
	if !entriesEnabled(rf) {
		return errors.New("the arguments recorded in the LaunchAgent disable every entry")
	}
	return nil
}

// serviceExitCode maps a service command error to an exit code: an invalid target application keeps 3, and everything
// else is a service management failure.
func serviceExitCode(err error) int {
	if errors.Is(err, appbundle.ErrInvalidApp) || errors.Is(err, errAppRequired) {
		return exitTarget
	}
	return exitService
}

// reportServiceError prints why a service command failed. When a multi-step command partly succeeded, it also prints
// the launchctl output, the completed and failed steps, the current state and the next step.
func reportServiceError(sub string, err error, stderr io.Writer) {
	var step *service.StepError
	if !errors.As(err, &step) {
		fmt.Fprintf(stderr, "findrelay service %s: %v\n", sub, err)
		return
	}
	fmt.Fprintf(stderr, "findrelay service %s: step %q failed: %v\n", sub, step.Failed, step.Err)
	var launchctlErr *service.LaunchctlError
	if errors.As(err, &launchctlErr) {
		for _, line := range strings.Split(strings.TrimRight(launchctlErr.Output, "\n"), "\n") {
			if line != "" {
				fmt.Fprintf(stderr, "  %s\n", line)
			}
		}
	}
	completed := strings.Join(step.Completed, ", ")
	if completed == "" {
		completed = "none"
	}
	fmt.Fprintf(stderr, "completed steps: %s\n", completed)
	fmt.Fprintln(stderr, step.Hint)
}
