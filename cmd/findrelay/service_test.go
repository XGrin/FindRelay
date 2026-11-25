package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/service"
	"github.com/XGrin/findrelay/internal/servicetest"
)

var (
	serviceDomain = service.GUIDomain(os.Getuid())
	serviceTarget = serviceDomain + "/" + service.AgentLabel
	callDisable   = "disable " + serviceTarget
	callEnable    = "enable " + serviceTarget
	callPrint     = "print " + serviceTarget
	callBootout   = "bootout " + serviceTarget
)

// newServiceEnv returns a service command test environment with a launchd stand-in, plus buffers that capture stdout
// and stderr.
func newServiceEnv(t *testing.T) (runtimeEnv, *servicetest.Launchd, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	base := t.TempDir()
	env, _ := newTestEnv(t, base)
	launchd := servicetest.NewLaunchd(serviceTarget)
	env.launchctl = launchd.Launchctl
	return env, launchd, &bytes.Buffer{}, &bytes.Buffer{}, base
}

func runService(env runtimeEnv, stdout, stderr *bytes.Buffer, args ...string) int {
	return runWithEnv(append([]string{"service"}, args...), stdout, stderr, env)
}

func mustRunDir(t *testing.T, env runtimeEnv) string {
	t.Helper()
	runDir, err := env.runDir()
	if err != nil {
		t.Fatalf("runDir: %v", err)
	}
	return runDir
}

// validAppPath creates a valid target application bundle in its own temporary directory.
func validAppPath(t *testing.T) string {
	t.Helper()
	return makeBundleForCLITest(t, t.TempDir(), "Bloom.app", "club.bloom.app")
}

// recordedExecutable returns an executable that really exists (the test binary itself), used as the findrelay
// recorded in the plist.
func recordedExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// installForTest writes a LaunchAgent definition directly, simulating the result of an earlier install.
func installForTest(t *testing.T, env runtimeEnv, programArguments ...string) {
	t.Helper()
	plistPath, err := env.agentPlistPath()
	if err != nil {
		t.Fatalf("agentPlistPath: %v", err)
	}
	if err := service.WriteAgentPlist(plistPath, service.AgentLabel, programArguments); err != nil {
		t.Fatalf("WriteAgentPlist: %v", err)
	}
}

func joined(lines []string) string { return strings.Join(lines, "\n") }

func TestServiceUsageErrors(t *testing.T) {
	env, launchd, stdout, stderr, base := newServiceEnv(t)
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "missing subcommand", code: exitUsage, want: "findrelay service: missing subcommand"},
		{name: "unknown subcommand", args: []string{"bogus"}, code: exitUsage, want: `findrelay service: unknown subcommand "bogus"`},
		{name: "start with positional argument", args: []string{"start", "extra"}, code: exitUsage, want: "findrelay service start: unexpected positional arguments"},
		{name: "status with unknown flag", args: []string{"status", "--verbose"}, code: exitUsage, want: "findrelay service status: flag provided but not defined"},
		{name: "install with unknown flag", args: []string{"install", "--app", validApp, "--nope"}, code: exitUsage, want: "findrelay service install: flag provided but not defined"},
		{name: "install without --app", args: []string{"install"}, code: exitTarget, want: `--app is required for active mode, e.g. findrelay service install --app "/Applications/<App>.app"`},
		{name: "install with a missing app", args: []string{"install", "--app", "/Applications/Missing.app"}, code: exitTarget, want: "findrelay service install: invalid target application"},
		{
			name: "install with every entry disabled", args: []string{"install", "--app", validApp, "--dock=false"}, code: exitUsage,
			want: "findrelay service install: both entries are disabled (--dock=false and no --desktop); the service would exit at once",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if code := runService(env, stdout, stderr, tt.args...); code != tt.code {
				t.Fatalf("service %q exit code = %d, want %d, stderr = %q", tt.args, code, tt.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("service %q stderr missing %q:\n%s", tt.args, tt.want, stderr.String())
			}
			// Usage errors come with the usage text; problems with the target application only give the reason,
			// consistent with run.
			if hasUsage := strings.Contains(stderr.String(), "Usage:"); hasUsage != (tt.code == exitUsage) {
				t.Errorf("service %q usage in stderr = %v for exit code %d:\n%s", tt.args, hasUsage, tt.code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("service %q stdout = %q, want empty", tt.args, stdout.String())
			}
		})
	}
	if calls := launchd.Calls(); len(calls) != 0 {
		t.Errorf("usage errors touched launchctl: %v", calls)
	}
}

// TestServiceHelpWritesUsageToStdout checks that --help is a normal usage query: the usage is written to stdout, the
// exit code is 0, and launchctl is not touched.
func TestServiceHelpWritesUsageToStdout(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"install", "--help"}, {"start", "-h"}, {"uninstall", "--help"}} {
		env, launchd, stdout, stderr, _ := newServiceEnv(t)
		if code := runService(env, stdout, stderr, args...); code != exitOK {
			t.Fatalf("service %q exit code = %d, want %d, stderr = %q", args, code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Usage:") {
			t.Fatalf("service %q stdout must contain the usage, got %q", args, stdout.String())
		}
		if stderr.Len() != 0 || len(launchd.Calls()) != 0 {
			t.Fatalf("service %q stderr = %q, launchctl calls = %v; want neither", args, stderr.String(), launchd.Calls())
		}
	}
}

// TestServiceInstallWritesCanonicalRunArguments checks that service install accepts the same arguments as run and
// writes them into the plist in canonical form; run reads the written arguments back unchanged.
func TestServiceInstallWritesCanonicalRunArguments(t *testing.T) {
	env, launchd, stdout, stderr, base := newServiceEnv(t)
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")

	args := []string{"install", "--app", validApp, "--desktop", "--open-command", `open -a Bloom "$folder"`,
		"--log-level", "info", "--dock-intercept", "down", "--desktop-intercept=down"}
	if code := runService(env, stdout, stderr, args...); code != exitOK {
		t.Fatalf("service install exit code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	if calls := launchd.Calls(); joined(calls) != callDisable {
		t.Fatalf("launchctl calls = %v, want only %q", calls, callDisable)
	}
	want := []string{"/usr/local/bin/findrelay", "run", "--app", validApp, "--desktop", "--open-command", `open -a Bloom "$folder"`,
		"--log-level", "info", "--dock-intercept=down", "--desktop-intercept=down"}
	plistPath, _ := env.agentPlistPath()
	def, err := service.ReadAgentDefinition(plistPath)
	if err != nil || def == nil || joined(def.ProgramArguments) != joined(want) {
		t.Fatalf("installed definition = (%+v, %v), want arguments %q", def, err, want)
	}
	if !strings.Contains(stdout.String(), "Run `findrelay service start` to start it now and allow login launch.") {
		t.Errorf("install output must explain how to start:\n%s", stdout.String())
	}

	// The canonical arguments are equivalent to themselves after being read back: the configuration run gets at
	// startup matches the one given at install.
	rf, err := parseRunFlags("run", runUsage, def.ProgramArguments[2:], stdout, stderr)
	if err != nil {
		t.Fatalf("parse the recorded arguments: %v", err)
	}
	if got := joined(rf.agentArguments(validApp)); got != joined(def.ProgramArguments[1:]) {
		t.Fatalf("recorded arguments are not canonical:\n got %q\nwant %q", got, def.ProgramArguments[1:])
	}

	// Without arguments, doctor reuses the target application recorded in the plist.
	stdout.Reset()
	if code := runWithEnv([]string{"doctor"}, stdout, stderr, env); code != exitOK {
		t.Fatalf("doctor after install: code = %d, stderr = %q", code, stderr.String())
	}
	for _, wantLine := range []string{"[Service]     INSTALLED", "args: " + strings.Join(want, " "), "[Target App]  OK (" + validApp} {
		if !strings.Contains(stdout.String(), wantLine) {
			t.Errorf("doctor output missing %q:\n%s", wantLine, stdout.String())
		}
	}
}

func TestServiceInstallOmitsDefaultArguments(t *testing.T) {
	env, launchd, stdout, stderr, base := newServiceEnv(t)
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	plistPath, _ := env.agentPlistPath()

	if code := runService(env, stdout, stderr, "install", "--app", validApp); code != exitOK {
		t.Fatalf("service install exit code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	def, err := service.ReadAgentDefinition(plistPath)
	want := "/usr/local/bin/findrelay run --app " + validApp
	if err != nil || def == nil || strings.Join(def.ProgramArguments, " ") != want {
		t.Fatalf("installed definition = (%+v, %v), want %q", def, err, want)
	}

	// Observe mode needs no target application; a repeated install only overwrites the arguments and does not touch
	// launchd.
	if code := runService(env, stdout, stderr, "install", "--observe", "--debug"); code != exitOK {
		t.Fatalf("service install --observe exit code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	def, _ = service.ReadAgentDefinition(plistPath)
	if strings.Join(def.ProgramArguments, " ") != "/usr/local/bin/findrelay run --observe --debug" {
		t.Fatalf("observe definition = %q", def.ProgramArguments)
	}
	if calls := launchd.Calls(); joined(calls) != callDisable {
		t.Fatalf("launchctl calls = %v; a reinstall must leave launchd alone", calls)
	}
}

func TestServiceInstallReportsLaunchctlFailure(t *testing.T) {
	env, launchd, stdout, stderr, base := newServiceEnv(t)
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	launchd.Fail = map[string]error{"disable": errors.New("exit status 5")}

	if code := runService(env, stdout, stderr, "install", "--app", validApp); code != exitService {
		t.Fatalf("service install exit code = %d, want %d, stderr = %q", code, exitService, stderr.String())
	}
	for _, want := range []string{
		`findrelay service install: step "disable" failed: launchctl disable ` + serviceTarget,
		"injected failure for disable",
		"completed steps: none",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
	plistPath, _ := env.agentPlistPath()
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist must not exist after a failed install, stat error = %v", err)
	}
}

// TestServiceLifecycle runs the full cycle of the six service commands: install without starting, start, status,
// repeated start, restart, stop and uninstall, plus the idempotency of repeated calls.
func TestServiceLifecycle(t *testing.T) {
	env, launchd, stdout, stderr, base := newServiceEnv(t)
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	plistPath, _ := env.agentPlistPath()
	// start checks that the executable recorded in the plist really exists, so install records the path of the test
	// binary itself.
	executable := recordedExecutable(t)
	env.selfExecutable = func() (string, error) { return executable, nil }
	expect := func(step string, args ...string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if code := runService(env, stdout, stderr, args...); code != exitOK {
			t.Fatalf("%s: service %q exit code = %d, want %d, stderr = %q", step, args, code, exitOK, stderr.String())
		}
	}
	expectOutput := func(step, want string) {
		t.Helper()
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("%s: output missing %q:\n%s", step, want, stdout.String())
		}
	}
	lastCalls := func(n int) string {
		t.Helper()
		calls := launchd.Calls()
		if len(calls) < n {
			t.Fatalf("launchctl calls = %v, want at least %d", calls, n)
		}
		return joined(calls[len(calls)-n:])
	}

	// Not installed is a normal state, so status returns successfully.
	expect("status before install", "status")
	for _, want := range []string{"Plist:        not installed", "Login launch: allowed", "launchd:      not loaded", "Function:     not effective"} {
		expectOutput("status before install", want)
	}

	// The first install only writes the plist and turns off login launch; it does not start the service.
	launchd.ResetCalls()
	expect("install", "install", "--app", validApp)
	if calls := launchd.Calls(); joined(calls) != callDisable || launchd.Loaded {
		t.Fatalf("install must only disable login launch: calls = %v loaded = %v", calls, launchd.Loaded)
	}

	// Start: enable first, then bootstrap; the start request has been submitted.
	expect("start", "start")
	if got := lastCalls(3); got != joined([]string{callEnable, callPrint, "bootstrap " + serviceDomain + " " + plistPath}) {
		t.Fatalf("launchctl calls for start = %q", got)
	}
	expectOutput("start", "login launch allowed.")
	expectOutput("start", "the start request was submitted.")

	// While an agent instance holds the single-instance lock, status reports the process running and the feature
	// being effective separately.
	instance, err := service.AcquireInstanceLock(mustRunDir(t, env), env.sessionInfo)
	if err != nil {
		t.Fatalf("AcquireInstanceLock: %v", err)
	}
	defer func() { _ = instance.Release() }()
	expect("status while running", "status")
	for _, want := range []string{"Plist:        installed", "Arguments:    " + executable + " run --app " + validApp, "Login launch: allowed",
		"Process:      running (pid 4242)", "Function:     effective"} {
		expectOutput("status while running", want)
	}

	// A repeated start keeps the running instance: no unload, and no new process.
	pid := launchd.PID
	expect("start again", "start")
	if got := lastCalls(2); got != joined([]string{callEnable, callPrint}) || launchd.PID != pid {
		t.Fatalf("second start must keep the running instance: calls = %v pid = %d", launchd.Calls(), launchd.PID)
	}
	expectOutput("start again", "nothing new was started; the running instance was kept.")

	// Restart: confirm that it is loaded and login launch is enabled, unload, confirm that it is unloaded, and load
	// again, without changing login launch.
	expect("restart", "restart")
	if got := lastCalls(5); got != joined([]string{callPrint, "print-disabled " + serviceDomain, callBootout, callPrint, "bootstrap " + serviceDomain + " " + plistPath}) {
		t.Fatalf("launchctl calls for restart = %q", got)
	}
	if launchd.PID == pid {
		t.Fatal("restart must start a new instance")
	}

	// Stop: turn off login launch and unload, keeping the plist.
	expect("stop", "stop")
	if got := lastCalls(4); got != joined([]string{callDisable, callPrint, callBootout, callPrint}) {
		t.Fatalf("launchctl calls for stop = %q", got)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("stop must keep the plist: %v", err)
	}
	expect("status after stop", "status")
	expectOutput("status after stop", "Login launch: disabled")
	expectOutput("status after stop", "launchd:      not loaded")

	// Stopping a stopped service keeps it stopped.
	expect("stop again", "stop")
	expectOutput("stop again", "The service is not loaded, so nothing is running.")

	// Uninstall: stop, then delete the plist; a repeated uninstall returns safely.
	expect("uninstall", "uninstall")
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist must be removed by uninstall, stat error = %v", err)
	}
	expect("uninstall again", "uninstall")
	expectOutput("uninstall again", "The LaunchAgent is not installed; nothing to remove.")
}

// TestServiceFailureExitCodes covers not installed, not loaded, launchd errors and a broken installed definition:
// each has its own exit code, and a failed validation does not touch launchd.
func TestServiceFailureExitCodes(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T, env runtimeEnv, launchd *servicetest.Launchd)
		args      []string
		code      int
		want      string
		wantCalls []string // expected call prefixes; empty means launchctl must not be touched
	}{
		{
			name: "start without install", args: []string{"start"}, code: exitService,
			want: "findrelay service start: LaunchAgent is not installed; run `findrelay service install --app <path>`",
		},
		{
			name: "restart when stopped", args: []string{"restart"}, code: exitService,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, recordedExecutable(t), "run", "--app", validAppPath(t))
			},
			want:      "findrelay service restart: LaunchAgent is not loaded in the GUI domain; run `findrelay service start`",
			wantCalls: []string{callPrint},
		},
		{
			name: "launchctl cannot bootstrap", args: []string{"start"}, code: exitService,
			prepare: func(t *testing.T, env runtimeEnv, launchd *servicetest.Launchd) {
				installForTest(t, env, recordedExecutable(t), "run", "--app", validAppPath(t))
				launchd.Fail = map[string]error{"bootstrap": errors.New("exit status 5")}
			},
			want:      `findrelay service start: step "bootstrap" failed`,
			wantCalls: []string{callEnable, callPrint},
		},
		{
			name: "recorded executable is missing", args: []string{"start"}, code: exitService,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, "/nonexistent/bin/findrelay", "run", "--app", validAppPath(t))
			},
			want: "findrelay service start: the executable recorded in the LaunchAgent is missing or not executable: /nonexistent/bin/findrelay",
		},
		{
			name: "recorded arguments do not name a target", args: []string{"start"}, code: exitTarget,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, recordedExecutable(t), "run")
			},
			want: "findrelay service start: the target application recorded in the LaunchAgent is invalid: --app is required for active mode",
		},
		{
			name: "recorded target application is gone", args: []string{"start"}, code: exitTarget,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, recordedExecutable(t), "run", "--app", "/Applications/Missing.app")
			},
			want: "findrelay service start: the target application recorded in the LaunchAgent is invalid: invalid target application",
		},
		{
			name: "recorded arguments disable every entry", args: []string{"start"}, code: exitService,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, recordedExecutable(t), "run", "--app", validAppPath(t), "--dock=false")
			},
			want: "findrelay service start: the arguments recorded in the LaunchAgent disable every entry",
		},
		{
			name: "plist does not hold a run command", args: []string{"start"}, code: exitService,
			prepare: func(t *testing.T, env runtimeEnv, _ *servicetest.Launchd) {
				installForTest(t, env, "/bin/ls")
			},
			want: "ProgramArguments must be the absolute executable path followed by `run` and its flags",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, launchd, stdout, stderr, _ := newServiceEnv(t)
			if tt.prepare != nil {
				tt.prepare(t, env, launchd)
			}
			launchd.ResetCalls()
			if code := runService(env, stdout, stderr, tt.args...); code != tt.code {
				t.Fatalf("service %q exit code = %d, want %d, stderr = %q", tt.args, code, tt.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr missing %q:\n%s", tt.want, stderr.String())
			}
			calls := launchd.Calls()
			if len(calls) < len(tt.wantCalls) || joined(calls[:len(tt.wantCalls)]) != joined(tt.wantCalls) {
				t.Errorf("launchctl calls = %v, want to start with %v", calls, tt.wantCalls)
			}
			if len(tt.wantCalls) == 0 && len(calls) != 0 {
				t.Errorf("launchctl calls = %v; a failed validation must not touch launchd", calls)
			}
		})
	}
}

// TestServiceReportsPartialSuccess checks that a partly successful multi-step command reports the completed steps,
// the failed step, the current state and the next step.
func TestServiceReportsPartialSuccess(t *testing.T) {
	env, launchd, stdout, stderr, _ := newServiceEnv(t)
	installForTest(t, env, recordedExecutable(t), "run", "--app", validAppPath(t))
	launchd.Fail = map[string]error{"bootstrap": errors.New("exit status 5")}

	if code := runService(env, stdout, stderr, "start"); code != exitService {
		t.Fatalf("service start exit code = %d, want %d, stderr = %q", code, exitService, stderr.String())
	}
	for _, want := range []string{
		`findrelay service start: step "bootstrap" failed: launchctl bootstrap ` + serviceDomain,
		"injected failure for bootstrap",
		"completed steps: enable",
		"Login launch is allowed, so the service will be started at the next login.",
		"`findrelay service stop` to disable login launch",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), "findrelay service start: login launch allowed.") {
		t.Errorf("the completed step must be reported on stdout:\n%s", stdout.String())
	}
}

func TestServiceCommandsNeedGUISession(t *testing.T) {
	env, launchd, _, stderr, _ := newServiceEnv(t)
	env.sessionInfo = func() (service.SessionInfo, error) {
		return service.SessionInfo{ID: 100015, HasGraphicAccess: false}, nil
	}
	for _, sub := range []string{"install", "start", "status", "restart", "stop", "uninstall"} {
		stderr.Reset()
		args := []string{sub}
		if sub == "install" {
			args = append(args, "--app", validAppPath(t))
		}
		if code := runService(env, &bytes.Buffer{}, stderr, args...); code != exitService {
			t.Errorf("service %s without a GUI session: exit code = %d, want %d", sub, code, exitService)
		}
		if !strings.Contains(stderr.String(), "GUI login session") {
			t.Errorf("service %s must explain the GUI session requirement, stderr = %q", sub, stderr.String())
		}
	}
	if calls := launchd.Calls(); len(calls) != 0 {
		t.Errorf("service commands without a GUI session touched launchctl: %v", calls)
	}
}

// TestRunAsLaunchAgentInstance covers an instance started by the LaunchAgent: diagnostics go to the injected
// background log instead of stderr, the permission hint points to the executable itself, and the exit reason is
// recorded.
func TestRunAsLaunchAgentInstance(t *testing.T) {
	// newAgentEnv returns a background-mode runtime environment; log collects the lines written to the system unified
	// log.
	newAgentEnv := func(t *testing.T) (runtimeEnv, *bytes.Buffer, chan struct{}, string) {
		t.Helper()
		base := t.TempDir()
		env, sigCh := newTestEnv(t, base)
		log := &bytes.Buffer{}
		env.serviceMode = func() bool { return true }
		env.serviceLog = func() io.WriteCloser {
			return nopWriteCloser{diagnostics.NewLineWriter(func(line string) {
				log.WriteString(line)
				log.WriteByte('\n')
			})}
		}
		return env, log, sigCh, base
	}
	expectLog := func(t *testing.T, log *bytes.Buffer, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(log.String(), want) {
				t.Errorf("agent log missing %q:\n%s", want, log.String())
			}
		}
	}

	t.Run("Accessibility not granted", func(t *testing.T) {
		env, log, _, base := newAgentEnv(t)
		app := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
		env.isAXTrusted = func(bool) bool { return false }
		var stdout, stderr bytes.Buffer
		if code := runWithEnv([]string{"run", "--app", app}, &stdout, &stderr, env); code != exitPermission {
			t.Fatalf("exit code = %d, want %d", code, exitPermission)
		}
		if stderr.Len() != 0 {
			t.Fatalf("background diagnostics must not go to stderr: %q", stderr.String())
		}
		expectLog(t, log,
			"findrelay run: service instance starting (dock=true desktop=false observe=false open=system log_level=warn)\n",
			"Accessibility permission is not granted. The LaunchAgent runs the findrelay executable itself: add that executable, not your terminal,",
			"findrelay run: exited with code 4\n")
		// The log is strictly redacted: it does not contain the target application path.
		if strings.Contains(log.String(), base) {
			t.Errorf("agent log leaked a path:\n%s", log.String())
		}
	})

	t.Run("another instance holds the session lock", func(t *testing.T) {
		env, log, _, base := newAgentEnv(t)
		app := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
		holder, err := service.AcquireInstanceLock(mustRunDir(t, env), env.sessionInfo)
		if err != nil {
			t.Fatalf("AcquireInstanceLock: %v", err)
		}
		defer func() { _ = holder.Release() }()
		var stdout, stderr bytes.Buffer
		if code := runWithEnv([]string{"run", "--app", app}, &stdout, &stderr, env); code != exitRuntime {
			t.Fatalf("exit code = %d, want %d", code, exitRuntime)
		}
		expectLog(t, log,
			"another findrelay instance is already running in this GUI session. The LaunchAgent instance exits; stop the other instance",
			"findrelay run: exited with code 6\n")
	})

	// An instance launched at login after the target application was moved or deleted: the log only states the
	// category and does not record the application path.
	t.Run("invalid target application", func(t *testing.T) {
		env, log, _, base := newAgentEnv(t)
		var stdout, stderr bytes.Buffer
		if code := runWithEnv([]string{"run", "--app", filepath.Join(base, "Private Apps", "Bloom.app")}, &stdout, &stderr, env); code != exitTarget {
			t.Fatalf("exit code = %d, want %d", code, exitTarget)
		}
		expectLog(t, log,
			"findrelay run: invalid target application; reinstall with `findrelay service install --app <path>`\n",
			"findrelay run: exited with code 3\n")
		if strings.Contains(log.String(), base) {
			t.Errorf("agent log leaked the target application path:\n%s", log.String())
		}
	})

	t.Run("graceful shutdown records the exit", func(t *testing.T) {
		env, log, sigCh, base := newAgentEnv(t)
		app := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
		env.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
			close(sigCh)
			return &mockTap{}, nil
		}
		var stdout, stderr bytes.Buffer
		if code := runWithEnv([]string{"run", "--app", app, "--debug"}, &stdout, &stderr, env); code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
		expectLog(t, log, "open=system log_level=debug", "findrelay run: exited with code 0\n")
		if strings.Contains(log.String(), app) {
			t.Errorf("agent log leaked the target application path:\n%s", log.String())
		}
	})
}

// TestForegroundRunKeepsTerminalHints checks that a foreground run keeps the original hints: the grantee is the
// terminal, and a single-instance conflict does not mention the LaunchAgent.
func TestForegroundRunKeepsTerminalHints(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")

	noAX, _ := newTestEnv(t, base)
	noAX.isAXTrusted = func(bool) bool { return false }
	var stderr bytes.Buffer
	if code := runWithEnv([]string{"run", "--app", validApp}, &bytes.Buffer{}, &stderr, noAX); code != exitPermission {
		t.Fatalf("exit code = %d, want %d", code, exitPermission)
	}
	if want := "Accessibility permission is not granted. Allow your terminal or findrelay in System Settings > Privacy & Security > Accessibility, then rerun.\n"; !strings.HasSuffix(stderr.String(), want) {
		t.Fatalf("foreground hint changed:\n%s", stderr.String())
	}

	conflict, _ := newTestEnv(t, base)
	holder, err := service.AcquireInstanceLock(mustRunDir(t, conflict), conflict.sessionInfo)
	if err != nil {
		t.Fatalf("AcquireInstanceLock: %v", err)
	}
	defer func() { _ = holder.Release() }()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", validApp}, &bytes.Buffer{}, &stderr, conflict); code != exitRuntime {
		t.Fatalf("exit code = %d, want %d", code, exitRuntime)
	}
	if stderr.String() != "findrelay run: another findrelay instance is already running in this GUI session\n" {
		t.Fatalf("foreground conflict message changed:\n%s", stderr.String())
	}
}
