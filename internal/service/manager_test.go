package service_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/service"
	"github.com/XGrin/findrelay/internal/servicetest"
)

const (
	testSessionID = 100015
	testLabel     = "io.findrelay.agent"

	callPrintDisabled = "print-disabled gui/501"
	callDisable       = "disable gui/501/" + testLabel
	callEnable        = "enable gui/501/" + testLabel
	callPrint         = "print gui/501/" + testLabel
	callBootout       = "bootout gui/501/" + testLabel
)

var program = []string{"/usr/local/bin/findrelay", "run", "--app", "/Applications/Bloom.app", "--desktop"}

// newManager builds a Manager that uses a launchd stand-in and temporary directories; the returned buffer collects
// the command output.
func newManager(t *testing.T, launchd *servicetest.Launchd) (*service.Manager, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	out := &bytes.Buffer{}
	m := service.NewManager("gui/501",
		filepath.Join(base, "LaunchAgents", testLabel+".plist"),
		filepath.Join(base, "run"),
		func() (service.SessionInfo, error) {
			return service.SessionInfo{ID: testSessionID, HasGraphicAccess: true}, nil
		},
		launchd.Launchctl,
	)
	m.ExitTimeout, m.PollInterval = 200*time.Millisecond, 5*time.Millisecond
	return m, out
}

func newLaunchd() *servicetest.Launchd {
	return servicetest.NewLaunchd("gui/501/" + testLabel)
}

func callBootstrap(m *service.Manager) string { return "bootstrap gui/501 " + m.PlistPath }

// line produces one field line of the status output, aligned the same way as writeStatus.
func line(label, value string) string { return fmt.Sprintf("  %-13s %s", label+":", value) }

func acceptAll(*service.AgentDefinition) error { return nil }

// install writes the LaunchAgent and then clears the stand-in's call log, so each test asserts only the calls of the
// command under test.
func install(t *testing.T, launchd *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
	t.Helper()
	if err := m.Install(program, out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	launchd.ResetCalls()
	out.Reset()
}

// presetService writes the plist directly and puts the stand-in into the state of a service the user has started:
// login launch allowed and loaded; running means the service process is still running. A started service must have
// been installed long before, so this skips install (install would first disable a service that is not installed).
func presetService(t *testing.T, launchd *servicetest.Launchd, m *service.Manager, running bool) {
	t.Helper()
	if err := service.WriteAgentPlist(m.PlistPath, testLabel, program); err != nil {
		t.Fatalf("WriteAgentPlist: %v", err)
	}
	disabled := false
	launchd.Disabled = &disabled
	launchd.Loaded, launchd.Running = true, running
	launchd.PID = 0
	if running {
		launchd.PID = 777
	}
	launchd.ResetCalls()
}

func assertCalls(t *testing.T, launchd *servicetest.Launchd, want ...string) {
	t.Helper()
	got := launchd.Calls()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("launchctl calls =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func readDefinition(t *testing.T, plistPath string) *service.AgentDefinition {
	t.Helper()
	def, err := service.ReadAgentDefinition(plistPath)
	if err != nil || def == nil {
		t.Fatalf("ReadAgentDefinition = (%+v, %v), want an installed definition", def, err)
	}
	return def
}

// holdInstance simulates an agent instance in the current GUI session that holds the single-instance lock, released
// when the test ends.
func holdInstance(t *testing.T, m *service.Manager) {
	t.Helper()
	lock, err := service.AcquireInstanceLock(m.RunDir, m.Session)
	if err != nil {
		t.Fatalf("AcquireInstanceLock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
}

func assertStepFailure(t *testing.T, err error, failed string, completed ...string) *service.StepError {
	t.Helper()
	var step *service.StepError
	if !errors.As(err, &step) {
		t.Fatalf("error = %v, want a *service.StepError", err)
	}
	if step.Failed != failed || strings.Join(step.Completed, ",") != strings.Join(completed, ",") {
		t.Fatalf("step failure = (%q, %q), want (%q, %q)", step.Failed, step.Completed, failed, completed)
	}
	if step.Hint == "" {
		t.Fatal("step failure must explain the current state")
	}
	return step
}

func TestInstallDisablesLoginLaunchAndWritesPlistWithoutStarting(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)

	if err := m.Install(program, out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// First install: disable first, then write the plist; no start action at all.
	assertCalls(t, launchd, callDisable)
	if launchd.Loaded || launchd.Disabled == nil || !*launchd.Disabled {
		t.Fatalf("launchd after install: loaded=%v disabled=%v, want not loaded and disabled", launchd.Loaded, launchd.Disabled)
	}
	def := readDefinition(t, m.PlistPath)
	if def.Label != testLabel || strings.Join(def.ProgramArguments, " ") != strings.Join(program, " ") {
		t.Fatalf("installed definition = %+v", def)
	}
	plist, err := os.ReadFile(m.PlistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	for _, want := range []string{
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<false/>",
		"<key>FINDRELAY_SERVICE</key>\n\t\t<string>1</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	for _, unwanted := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(string(plist), unwanted) {
			t.Errorf("plist must leave the job's standard streams to launchd, found %s", unwanted)
		}
	}
	if info, err := os.Stat(m.PlistPath); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("plist mode = %v, %v; want 0644", info, err)
	}
	for _, want := range []string{"findrelay service install: login launch disabled.", "LaunchAgent written with arguments: run --app /Applications/Bloom.app --desktop."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install output missing %q:\n%s", want, out.String())
		}
	}
	// No temporary file is left in the LaunchAgents directory.
	entries, err := os.ReadDir(filepath.Dir(m.PlistPath))
	if err != nil || len(entries) != 1 {
		t.Errorf("LaunchAgents entries = %v, %v; want only the plist", entries, err)
	}
}

func TestReinstallReplacesArgumentsAndKeepsStartStopState(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	install(t, launchd, m, out)

	// The user has started the service: login launch allowed, loaded and running.
	launchd.Disabled = new(bool)
	launchd.Loaded, launchd.Running, launchd.PID = true, true, 777
	updated := []string{"/usr/local/bin/findrelay", "run", "--app", "/Applications/FileOS.app", "--dock=false", "--desktop"}
	if err := m.Install(updated, out); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	// A repeated install does not touch launchd and keeps the started or stopped state as it is.
	assertCalls(t, launchd)
	if got := strings.Join(readDefinition(t, m.PlistPath).ProgramArguments, " "); got != strings.Join(updated, " ") {
		t.Fatalf("program arguments after reinstall = %s", got)
	}
	if !launchd.Loaded || !launchd.Running || launchd.PID != 777 || *launchd.Disabled {
		t.Fatalf("reinstall changed the service state: loaded=%v running=%v pid=%d disabled=%v",
			launchd.Loaded, launchd.Running, launchd.PID, *launchd.Disabled)
	}
}

func TestInstallFailureWritesNothing(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"disable": errors.New("launchd unavailable")}
	m, _ := newManager(t, launchd)

	err := m.Install(program, &bytes.Buffer{})
	assertStepFailure(t, err, "disable")
	if _, statErr := os.Stat(m.PlistPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("plist must not exist after a failed install, stat error = %v", statErr)
	}
}

// TestInstallReportsDisabledLoginLaunchWhenThePlistCannotBeWritten covers a first install where disable has taken
// effect but the plist was not written: the hint must state truthfully that login launch has been turned off.
func TestInstallReportsDisabledLoginLaunchWhenThePlistCannotBeWritten(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	// LaunchAgents is a dangling symlink: reading the plist reports that it does not exist, but the directory cannot
	// be written to.
	launchAgents := filepath.Join(t.TempDir(), "LaunchAgents")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), launchAgents); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	m.PlistPath = filepath.Join(launchAgents, testLabel+".plist")

	step := assertStepFailure(t, m.Install(program, out), "write plist", "disable")
	if !strings.Contains(step.Hint, "Login launch is disabled") {
		t.Errorf("hint must say that login launch is disabled, got %q", step.Hint)
	}
	if launchd.Disabled == nil || !*launchd.Disabled {
		t.Fatal("disable must have taken effect")
	}
}

func TestStartBootstrapsWhenNotLoaded(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	install(t, launchd, m, out)

	if err := m.Start(acceptAll, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertCalls(t, launchd, callEnable, callPrint, callBootstrap(m))
	if !launchd.Loaded || !launchd.Running || launchd.Disabled == nil || *launchd.Disabled {
		t.Fatalf("launchd after start: loaded=%v running=%v disabled=%+v", launchd.Loaded, launchd.Running, launchd.Disabled)
	}
	for _, want := range []string{"login launch allowed.", "the start request was submitted.", "`findrelay service status`"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("start output missing %q:\n%s", want, out.String())
		}
	}
	// A submitted start request must not be described as the process running or the tap being ready; it can only
	// point the user to status to verify.
	for _, claim := range []string{"tap is ready", "is effective", "is running"} {
		if strings.Contains(out.String(), claim) {
			t.Errorf("start must not claim %q:\n%s", claim, out.String())
		}
	}
}

// TestStartKeepsRunningInstance covers a loaded service with a running instance: no unload, no new bootstrap, and no
// new process.
func TestStartKeepsRunningInstance(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	holdInstance(t, m)

	if err := m.Start(acceptAll, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertCalls(t, launchd, callEnable, callPrint)
	if launchd.PID != 777 || !launchd.Running {
		t.Fatalf("running instance was replaced: pid=%d running=%v", launchd.PID, launchd.Running)
	}
	if !strings.Contains(out.String(), "The session lock in GUI session 100015 is already held, so nothing new was started; the running instance was kept.") {
		t.Errorf("start output missing the kept-instance notice:\n%s", out.String())
	}
}

// TestStartStartsNothingWhileTheSessionLockIsHeld covers a loaded service whose process has exited while a foreground
// `findrelay run` holds the session lock: start only allows login launch and does not launch a service process that
// is bound to exit on a single-instance conflict.
func TestStartStartsNothingWhileTheSessionLockIsHeld(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, false)
	launchd.LastExit = "6"
	holdInstance(t, m)

	if err := m.Start(acceptAll, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertCalls(t, launchd, callEnable, callPrint)
	if launchd.Running {
		t.Fatal("start started a service process although the session lock is already held")
	}
	if launchd.Disabled == nil || *launchd.Disabled {
		t.Fatal("start must still allow login launch")
	}
}

// TestStartReloadsLoadedServiceWithoutRunningInstance covers a loaded service with no running instance: kickstart
// would reuse the old definition from bootstrap time, so start must unload first and then bootstrap.
func TestStartReloadsLoadedServiceWithoutRunningInstance(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, false)
	launchd.LastExit = "6"
	launchd.UnloadDelay = 2

	if err := m.Start(acceptAll, out); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertCalls(t, launchd, callEnable, callPrint, callBootout, callPrint, callPrint, callPrint, callBootstrap(m))
	if !launchd.Running {
		t.Fatal("the service must be running again after bootstrap")
	}
}

func TestStartFailsBeforeTouchingLaunchdWhenServiceIsNotReady(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)

	if err := m.Start(acceptAll, out); !errors.Is(err, service.ErrServiceNotInstalled) {
		t.Fatalf("start without install: error = %v, want ErrServiceNotInstalled", err)
	}
	install(t, launchd, m, out)
	validation := errors.New("recorded target application is invalid")
	if err := m.Start(func(*service.AgentDefinition) error { return validation }, out); !errors.Is(err, validation) {
		t.Fatalf("start with invalid definition: error = %v, want the validation error", err)
	}
	// Neither failed start touched launchd.
	assertCalls(t, launchd)
}

func TestStartReportsPartialSuccess(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"bootstrap": errors.New("exit status 5")}
	m, out := newManager(t, launchd)
	install(t, launchd, m, out)

	err := m.Start(acceptAll, out)
	step := assertStepFailure(t, err, "bootstrap", "enable")
	if !strings.Contains(step.Hint, "next login") {
		t.Errorf("hint must describe the enabled login launch, got %q", step.Hint)
	}
	// The step that allows login launch has already taken effect.
	if launchd.Disabled == nil || *launchd.Disabled {
		t.Fatal("login launch must stay allowed after a failed bootstrap")
	}
	if !strings.Contains(out.String(), "login launch allowed.") {
		t.Errorf("completed step must be reported on stdout:\n%s", out.String())
	}
}

func TestStartStopsAfterFailedEnable(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"enable": errors.New("launchd unavailable")}
	m, out := newManager(t, launchd)
	install(t, launchd, m, out)

	assertStepFailure(t, m.Start(acceptAll, out), "enable")
	assertCalls(t, launchd, callEnable)
}

func TestRestartReloadsLoadedServiceAndKeepsLoginLaunchSetting(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)

	if err := m.Restart(acceptAll, out); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// Confirm that it is loaded and login launch is enabled, then bootout, confirm that it is unloaded, and bootstrap
	// again; no enable or disable at any point.
	assertCalls(t, launchd, callPrint, callPrintDisabled, callBootout, callPrint, callBootstrap(m))
	if launchd.Disabled == nil || *launchd.Disabled {
		t.Fatalf("restart changed the login launch setting to disabled=%+v", launchd.Disabled)
	}
	if launchd.PID == 777 {
		t.Error("restart must start a new instance")
	}
}

// TestRestartKeepsARunningServiceWhenLoginLaunchIsDisabled covers a service that is still running while login launch
// is off (for example, stop only completed disable): launchd refuses to load a disabled service, so restart returns
// before unloading and points to start, leaving the running instance unaffected.
func TestRestartKeepsARunningServiceWhenLoginLaunchIsDisabled(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	disabled := true
	launchd.Disabled = &disabled

	err := m.Restart(acceptAll, out)
	if !errors.Is(err, service.ErrLoginLaunchDisabled) || !strings.Contains(err.Error(), "`findrelay service start`") {
		t.Fatalf("Restart with login launch disabled: error = %v, want ErrLoginLaunchDisabled pointing to start", err)
	}
	assertCalls(t, launchd, callPrint, callPrintDisabled)
	if !launchd.Loaded || !launchd.Running || launchd.PID != 777 {
		t.Fatalf("restart must leave the running instance alone: loaded=%v running=%v pid=%d", launchd.Loaded, launchd.Running, launchd.PID)
	}
}

// TestRestartReportsLoginLaunchQueryFailure checks that restart also leaves the service alone when the login launch
// state cannot be read.
func TestRestartReportsLoginLaunchQueryFailure(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"print-disabled": errors.New("launchd unavailable")}
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)

	assertStepFailure(t, m.Restart(acceptAll, out), "inspect")
	assertCalls(t, launchd, callPrint, callPrintDisabled)
	if !launchd.Loaded || !launchd.Running {
		t.Fatal("the service must keep running when the login launch state cannot be read")
	}
}

// TestRestartKeepsStoppedState covers restart on a stopped or not yet installed service: the state stays as it is,
// and the matching action is suggested.
func TestRestartKeepsStoppedState(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)

	if err := m.Restart(acceptAll, out); !errors.Is(err, service.ErrServiceNotInstalled) {
		t.Fatalf("restart without install: error = %v, want ErrServiceNotInstalled", err)
	}
	assertCalls(t, launchd)

	install(t, launchd, m, out)
	if err := m.Restart(acceptAll, out); !errors.Is(err, service.ErrServiceNotLoaded) {
		t.Fatalf("restart on a stopped service: error = %v, want ErrServiceNotLoaded", err)
	}
	assertCalls(t, launchd, callPrint)
	if launchd.Loaded || launchd.Disabled == nil || !*launchd.Disabled {
		t.Fatalf("restart on a stopped service changed the state: loaded=%v disabled=%+v", launchd.Loaded, launchd.Disabled)
	}
}

func TestRestartWaitsForOldInstanceToLeaveBeforeBootstrap(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	launchd.UnloadDelay = 3

	if err := m.Restart(acceptAll, out); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	calls := launchd.Calls()
	bootout, bootstrap := -1, -1
	loadedPrints := 0
	for i, call := range calls {
		switch call {
		case callBootout:
			bootout = i
		case callBootstrap(m):
			bootstrap = i
		}
		if bootout >= 0 && bootstrap < 0 && call == callPrint {
			loadedPrints++
		}
	}
	// The 3 prints after bootout, while the old instance is still there, all report loaded; only then comes
	// bootstrap.
	if bootout < 0 || bootstrap < 0 || loadedPrints != 4 {
		t.Fatalf("calls = %v; want %d loaded prints between bootout and bootstrap", calls, 4)
	}
}

func TestRestartGivesUpWhenOldInstanceDoesNotExit(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	launchd.NeverUnload = true

	err := m.Restart(acceptAll, out)
	step := assertStepFailure(t, err, "wait for exit")
	if !errors.Is(step, service.ErrServiceStillLoaded) {
		t.Fatalf("error = %v, want ErrServiceStillLoaded", err)
	}
	// Before the timeout it only queries launchd repeatedly: no bootstrap, and no change to the login launch setting.
	calls := launchd.Calls()
	if len(calls) < 4 || calls[0] != callPrint || calls[1] != callPrintDisabled || calls[2] != callBootout {
		t.Fatalf("calls = %v; want the loaded and login launch checks, bootout, then polling", calls)
	}
	for _, call := range calls[3:] {
		if call != callPrint {
			t.Fatalf("unexpected call while waiting for the old instance: %q in %v", call, calls)
		}
	}
}

func TestRestartReportsBootstrapFailureAfterUnload(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	launchd.Fail = map[string]error{"bootstrap": errors.New("exit status 5")}

	err := m.Restart(acceptAll, out)
	step := assertStepFailure(t, err, "bootstrap", "bootout")
	if !strings.Contains(step.Hint, "unloaded") {
		t.Errorf("hint must say the service is unloaded, got %q", step.Hint)
	}
}

func TestStopIsIdempotentAndKeepsPlist(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)

	if err := m.Stop(out); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertCalls(t, launchd, callDisable, callPrint, callBootout, callPrint)
	if launchd.Loaded || launchd.Running || launchd.Disabled == nil || !*launchd.Disabled {
		t.Fatalf("launchd after stop: loaded=%v running=%v disabled=%+v", launchd.Loaded, launchd.Running, launchd.Disabled)
	}
	readDefinition(t, m.PlistPath)

	// A stopped service stays stopped.
	out.Reset()
	if err := m.Stop(out); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	assertCalls(t, launchd, callDisable, callPrint, callBootout, callPrint, callDisable, callPrint)
	if !strings.Contains(out.String(), "The service is not loaded, so nothing is running.") {
		t.Errorf("second stop output:\n%s", out.String())
	}
}

func TestStopOnUninstalledServiceSucceeds(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	if err := m.Stop(out); err != nil {
		t.Fatalf("Stop without install: %v", err)
	}
	assertCalls(t, launchd, callDisable, callPrint)
}

func TestStopReportsFailedBootout(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	launchd.Fail = map[string]error{"bootout": errors.New("exit status 3")}

	assertStepFailure(t, m.Stop(out), "bootout", "disable")
	if !launchd.Loaded || !launchd.Running {
		t.Fatal("the service must still be running after a failed bootout")
	}
}

func TestUninstallStopsThenRemovesOwnPlist(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)

	if err := m.Uninstall(out); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	// Stop first (disable, then bootout), then delete the plist.
	assertCalls(t, launchd, callDisable, callPrint, callBootout, callPrint)
	if _, err := os.Stat(m.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still exists after uninstall, stat error = %v", err)
	}
	if !strings.Contains(out.String(), "findrelay service uninstall: LaunchAgent removed.") {
		t.Errorf("uninstall output:\n%s", out.String())
	}

	// A repeated uninstall returns safely without calling launchctl again.
	out.Reset()
	if err := m.Uninstall(out); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	assertCalls(t, launchd, callDisable, callPrint, callBootout, callPrint)
	if !strings.Contains(out.String(), "not installed; nothing to remove") {
		t.Errorf("second uninstall output:\n%s", out.String())
	}
}

func TestUninstallKeepsPlistWhenStoppingFails(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	presetService(t, launchd, m, true)
	launchd.Fail = map[string]error{"disable": errors.New("launchd unavailable")}

	err := m.Uninstall(out)
	if step := assertStepFailure(t, err, "disable"); step.Command != "uninstall" {
		t.Fatalf("failed step belongs to command %q, want uninstall", step.Command)
	}
	// When stop fails, the plist is not deleted; the service is still running.
	readDefinition(t, m.PlistPath)
	assertCalls(t, launchd, callDisable)
	if !launchd.Loaded || !launchd.Running {
		t.Fatal("the service must still be running when its stop step failed")
	}
}

func TestUninstallRefusesPlistOfAnotherService(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	if err := service.WriteAgentPlist(m.PlistPath, "com.example.other", program); err != nil {
		t.Fatalf("WriteAgentPlist: %v", err)
	}

	if err := m.Uninstall(out); !errors.Is(err, service.ErrForeignAgentPlist) {
		t.Fatalf("Uninstall of a foreign plist: error = %v, want ErrForeignAgentPlist", err)
	}
	readDefinition(t, m.PlistPath)
	assertCalls(t, launchd)
}

// TestStartAndRestartRefusePlistOfAnotherService covers a plist path that holds another service's definition: start
// and restart do not load it and do not touch launchd.
func TestStartAndRestartRefusePlistOfAnotherService(t *testing.T) {
	launchd := newLaunchd()
	m, out := newManager(t, launchd)
	if err := service.WriteAgentPlist(m.PlistPath, "com.example.other", program); err != nil {
		t.Fatalf("WriteAgentPlist: %v", err)
	}

	for name, command := range map[string]func() error{
		"start":   func() error { return m.Start(acceptAll, out) },
		"restart": func() error { return m.Restart(acceptAll, out) },
	} {
		if err := command(); !errors.Is(err, service.ErrForeignAgentPlist) {
			t.Errorf("%s with a foreign plist: error = %v, want ErrForeignAgentPlist", name, err)
		}
	}
	assertCalls(t, launchd)
}

func TestStatusSeparatesProcessFromFunction(t *testing.T) {
	type testCase struct {
		name     string
		prepare  func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer)
		holdLock bool
		want     []string
		notWant  []string
	}
	installOnly := func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
		install(t, l, m, out)
	}
	tests := []testCase{
		{
			// A service that was never installed has no entry in launchd's override table, which by launchd's
			// semantics means login launch is allowed.
			name: "not installed",
			want: []string{line("Plist", "not installed"), line("Login launch", "allowed"), line("launchd", "not loaded in gui/501"),
				line("Process", "none (the service is not loaded)"), line("Function", "not effective"), "`findrelay service install --app <path>`"},
		},
		{
			name:    "installed but never started",
			prepare: installOnly,
			want:    []string{line("Plist", "installed"), line("Arguments", "") + strings.Join(program, " "), line("Login launch", "disabled"), "Run `findrelay service start`"},
		},
		{
			name: "running instance holds the session lock",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Disabled = new(bool)
				l.Loaded, l.Running, l.PID = true, true, 4242
			},
			holdLock: true,
			want:     []string{line("Login launch", "allowed"), line("launchd", "loaded in gui/501"), line("Process", "running (pid 4242)"), line("Instance", "active in GUI session 100015"), line("Function", "effective")},
			// When the feature is effective, there is no explanation line other than the agent log query hint.
			notWant: []string{"not effective", "Run `findrelay service"},
		},
		{
			name: "agent exited for missing Accessibility permission",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Disabled = new(bool)
				l.Loaded, l.LastExit = true, "4"
			},
			want: []string{line("Process", "not running (exit code 4)"), line("Function", "not effective"),
				"Accessibility permission is not granted to the findrelay executable that launchd starts", "not your terminal"},
		},
		{
			name: "agent exited for an instance conflict",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Loaded, l.LastExit = true, "6"
			},
			want: []string{line("Process", "not running (exit code 6)"), line("Function", "not effective"), "Another instance already held the session lock"},
		},
		{
			name: "agent was terminated by a signal",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Loaded, l.LastSignal = true, "Terminated: 15"
			},
			want: []string{line("Process", "not running (signal Terminated: 15)"), "terminated by signal Terminated: 15"},
		},
		{
			name:     "foreground instance holds the session lock while the service is stopped",
			prepare:  installOnly,
			holdLock: true,
			want: []string{line("Process", "none (the service is not loaded)"), line("Function", "not effective"),
				line("Instance", "active in GUI session 100015"), "held by an instance the service did not start"},
		},
		{
			name: "foreground instance holds the session lock while the service is loaded",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Loaded, l.LastExit = true, "6"
			},
			holdLock: true,
			want:     []string{line("Process", "not running (exit code 6)"), "held by an instance the service did not start"},
		},
		{
			name: "process alive without the session lock",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Loaded, l.Running, l.PID = true, true, 4242
			},
			want:    []string{line("Process", "running (pid 4242)"), line("Function", "not effective"), "does not hold the session lock"},
			notWant: []string{line("Function", "effective")},
		},
		{
			name: "launchctl print output is not understood",
			prepare: func(t *testing.T, l *servicetest.Launchd, m *service.Manager, out *bytes.Buffer) {
				install(t, l, m, out)
				l.Loaded = true
				l.PrintText = "io.findrelay.agent = { unexpected layout }\n"
			},
			want: []string{line("Process", "unknown"), line("Function", "unknown"), "io.findrelay.agent = { unexpected layout }"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launchd := newLaunchd()
			m, out := newManager(t, launchd)
			if tt.prepare != nil {
				tt.prepare(t, launchd, m, out)
			}
			if tt.holdLock {
				holdInstance(t, m)
			}
			out.Reset()
			if err := m.Status(out); err != nil {
				t.Fatalf("Status: %v", err)
			}
			got := out.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("status output missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("status output must not contain %q:\n%s", notWant, got)
				}
			}
			// status only queries and changes no state.
			assertCalls(t, launchd, callPrintDisabled, callPrint)
		})
	}
}

func TestStatusFailsWhenLaunchdCannotBeQueried(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"print-disabled": errors.New("launchd unavailable")}
	m, _ := newManager(t, launchd)

	if err := m.Status(&bytes.Buffer{}); err == nil {
		t.Fatal("Status must fail when the login launch state cannot be read")
	}
}

func TestStartReportsLaunchdQueryFailure(t *testing.T) {
	launchd := newLaunchd()
	launchd.Fail = map[string]error{"print": errors.New("exit status 1")}
	m, out := newManager(t, launchd)
	install(t, launchd, m, out)

	assertStepFailure(t, m.Start(acceptAll, out), "inspect", "enable")
}

func TestServiceCommandsRequireGUISession(t *testing.T) {
	launchd := newLaunchd()
	m, _ := newManager(t, launchd)
	m.Session = func() (service.SessionInfo, error) {
		return service.SessionInfo{ID: testSessionID, HasGraphicAccess: false}, nil
	}

	commands := map[string]func() error{
		"install":   func() error { return m.Install(program, &bytes.Buffer{}) },
		"start":     func() error { return m.Start(acceptAll, &bytes.Buffer{}) },
		"status":    func() error { return m.Status(&bytes.Buffer{}) },
		"restart":   func() error { return m.Restart(acceptAll, &bytes.Buffer{}) },
		"stop":      func() error { return m.Stop(&bytes.Buffer{}) },
		"uninstall": func() error { return m.Uninstall(&bytes.Buffer{}) },
	}
	for name, command := range commands {
		if err := command(); !errors.Is(err, service.ErrNoGUISession) {
			t.Errorf("%s without a GUI session: error = %v, want ErrNoGUISession", name, err)
		}
	}
	assertCalls(t, launchd)
}

// TestServiceCommandsAreSerialized checks that service commands are serialized: while another service command holds
// the operations lock, a later command waits instead of interleaving with it.
func TestServiceCommandsAreSerialized(t *testing.T) {
	launchd := newLaunchd()
	m, _ := newManager(t, launchd)
	holder, err := service.AcquireOperationsLock(m.RunDir)
	if err != nil {
		t.Fatalf("AcquireOperationsLock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Release() })

	done := make(chan error, 1)
	go func() { done <- m.Stop(&bytes.Buffer{}) }()
	select {
	case err := <-done:
		t.Fatalf("Stop finished while another service command held the operations lock (error = %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	assertCalls(t, launchd)

	if err := holder.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not run after the operations lock was released")
	}
	assertCalls(t, launchd, callDisable, callPrint)
}

// TestStatusIgnoresLabelsThatContainTheQueriedLabel covers an override table that may hold other services whose names
// contain this service's label (such as io.findrelay.agent.helper); such an entry must not be mistaken for this
// service's entry. The stand-in emits entries under its own label io.findrelay.agent, and here the Manager queries
// its prefix io.findrelay.
func TestStatusIgnoresLabelsThatContainTheQueriedLabel(t *testing.T) {
	launchd := newLaunchd()
	disabled := true
	launchd.Disabled = &disabled
	m, out := newManager(t, launchd)
	m.Label = "io.findrelay"

	if err := m.Status(out); err != nil {
		t.Fatalf("Status with a longer label in the override table: %v", err)
	}
	if !strings.Contains(out.String(), line("Login launch", "allowed")) {
		t.Errorf("another service's entry must not count as this service's:\n%s", out.String())
	}
}

func TestParseDisabledTable(t *testing.T) {
	// Cover the three override table states through the Login launch line of status.
	for _, tt := range []struct {
		name    string
		entries *bool
		want    string
	}{
		{name: "no entry means allowed", entries: nil, want: line("Login launch", "allowed")},
		{name: "explicitly allowed", entries: new(bool), want: line("Login launch", "allowed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			launchd := newLaunchd()
			launchd.Disabled = tt.entries
			m, out := newManager(t, launchd)
			if err := m.Status(out); err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("status output missing %q:\n%s", tt.want, out.String())
			}
		})
	}
}
