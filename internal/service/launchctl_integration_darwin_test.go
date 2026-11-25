package service_test

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/service"
)

// TestMain lets the test binary double as the stand-in agent started by the LaunchAgent: the FINDRELAY_SERVICE
// environment variable written by the plist appears only in that case.
func TestMain(m *testing.M) {
	if os.Getenv(service.ServiceEnvironmentKey) == service.ServiceEnvironmentValue {
		os.Exit(runStandInAgent())
	}
	os.Exit(m.Run())
}

// runStandInAgent acts as the agent started by the LaunchAgent: it takes the single-instance lock of the current GUI
// session and, after receiving SIGTERM, delays before exiting, so that the bounded waits of restart and stop really
// wait for the old instance to leave on a real machine.
func runStandInAgent() int {
	runDir := ""
	for i, arg := range os.Args[1:] {
		if arg == "--run-dir" && i+2 < len(os.Args) {
			runDir = os.Args[i+2]
		}
	}
	lock, err := service.AcquireInstanceLock(runDir, darwin.CurrentGUISession)
	if err != nil {
		return service.ExitRuntime
	}
	defer func() { _ = lock.Release() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	time.Sleep(300 * time.Millisecond)
	return service.ExitOK
}

// TestLaunchctlIntegration runs the LaunchAgent lifecycle with the real launchctl and checks the real-machine
// behavior the state machine depends on: the first install only disables and does not load; after start the process
// is alive and holds the session lock; a repeated start keeps the original process; restart waits for the old
// instance to exit and then gets a new process; after stop the service is gone from the domain and the lock is
// released; uninstall deletes the plist. It runs only when FINDRELAY_LAUNCHCTL_INTEGRATION=1 is set explicitly, and
// it is skipped when a real service is already installed or loaded on this machine, so the user's configuration is
// not touched.
func TestLaunchctlIntegration(t *testing.T) {
	if os.Getenv("FINDRELAY_LAUNCHCTL_INTEGRATION") != "1" {
		t.Skip("set FINDRELAY_LAUNCHCTL_INTEGRATION=1 to drive the real launchctl")
	}
	target := service.GUIDomain(os.Getuid()) + "/" + service.AgentLabel
	if _, err := service.Launchctl("print", target); err == nil {
		t.Skip("io.findrelay.agent is loaded in the GUI domain; uninstall the real service before running this test")
	}
	if realPlist, err := service.DefaultAgentPlistPath(); err != nil {
		t.Skipf("cannot locate the real LaunchAgent path: %v", err)
	} else if def, err := service.ReadAgentDefinition(realPlist); err != nil || def != nil {
		t.Skip("a real io.findrelay.agent LaunchAgent is installed; uninstall it before running this test")
	}

	sess, err := darwin.CurrentGUISession()
	if err != nil || sess.ID == 0 || !sess.HasGraphicAccess {
		t.Skipf("a GUI login session is required (session=%+v err=%v)", sess, err)
	}
	base := t.TempDir()
	runDir := filepath.Join(base, "run")
	m := service.NewManager(service.GUIDomain(os.Getuid()), filepath.Join(base, service.AgentLabel+".plist"), runDir,
		darwin.CurrentGUISession, service.Launchctl)
	m.PollInterval = 10 * time.Millisecond

	// Whichever step the test fails at, never leave a loaded service in the user's launchd domain.
	t.Cleanup(func() {
		_, _ = service.Launchctl("bootout", target)
		_, _ = service.Launchctl("disable", target)
	})

	assertUnloaded := func(step string) {
		t.Helper()
		_, err := service.Launchctl("print", target)
		var launchctlErr *service.LaunchctlError
		if !errors.As(err, &launchctlErr) || launchctlErr.Code != service.LaunchctlExitNotFound {
			t.Fatalf("%s: launchctl print error = %v, want the not-found exit code %d", step, err, service.LaunchctlExitNotFound)
		}
		if running, err := service.ProbeInstanceRunning(runDir, sess.ID); err != nil || running {
			t.Fatalf("%s: session lock probe = (%v, %v), want released", step, running, err)
		}
	}
	assertLoginLaunch := func(step string, want string) {
		t.Helper()
		out, err := service.Launchctl("print-disabled", m.Domain)
		if err != nil {
			t.Fatalf("%s: launchctl print-disabled: %v", step, err)
		}
		if !strings.Contains(out, "\""+service.AgentLabel+"\" => "+want) {
			t.Fatalf("%s: override table does not report %s for the service:\n%s", step, want, out)
		}
	}
	statusPID := func(step string) string {
		t.Helper()
		out := &strings.Builder{}
		if err := m.Status(out); err != nil {
			t.Fatalf("%s: Status: %v", step, err)
		}
		if !strings.Contains(out.String(), "Function:     effective") {
			t.Fatalf("%s: status must report the function as effective:\n%s", step, out.String())
		}
		pid := servicePID.FindStringSubmatch(out.String())
		if pid == nil {
			t.Fatalf("%s: status does not report a running process:\n%s", step, out.String())
		}
		return pid[1]
	}
	// waitLocked waits until the stand-in agent holds the session lock, which means it is running as the service
	// process.
	waitLocked := func(step string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if running, err := service.ProbeInstanceRunning(runDir, sess.ID); err == nil && running {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the service process did not take the session lock in time", step)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	program := []string{os.Args[0], "run", "--run-dir", runDir}

	// First install: only disable login launch, do not load.
	if err := m.Install(program, &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertUnloaded("after install")
	assertLoginLaunch("after install", "disabled")

	// Start: after bootstrap the process is alive and holds the session lock, and status reports the feature as
	// effective.
	if err := m.Start(func(*service.AgentDefinition) error { return nil }, &strings.Builder{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitLocked("after start")
	first := statusPID("after start")
	assertLoginLaunch("after start", "enabled")

	// A repeated start keeps the running process.
	if err := m.Start(func(*service.AgentDefinition) error { return nil }, &strings.Builder{}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if second := statusPID("after second start"); second != first {
		t.Fatalf("second start replaced the running instance: pid %s -> %s", first, second)
	}

	// Restart: after bootout the stand-in takes 300ms to exit, and restart must wait for it to leave before launching
	// a new process.
	restarted := time.Now()
	if err := m.Restart(func(*service.AgentDefinition) error { return nil }, &strings.Builder{}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if elapsed := time.Since(restarted); elapsed < 300*time.Millisecond {
		t.Fatalf("Restart returned after %v, before the old instance could exit", elapsed)
	}
	waitLocked("after restart")
	if third := statusPID("after restart"); third == first {
		t.Fatalf("restart kept the old instance (pid %s)", third)
	}

	// Stop: the service is gone from the domain, the session lock is released, and login launch is off.
	if err := m.Stop(&strings.Builder{}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertUnloaded("after stop")
	assertLoginLaunch("after stop", "disabled")

	// Uninstall: delete the plist after stopping.
	if err := m.Uninstall(&strings.Builder{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(m.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still exists after uninstall, stat error = %v", err)
	}
	assertUnloaded("after uninstall")
}

var servicePID = regexp.MustCompile(`Process:\s+running \(pid (\d+)\)`)
