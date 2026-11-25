package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
	"github.com/XGrin/findrelay/internal/relay"
	"github.com/XGrin/findrelay/internal/service"
)

func makeBundleForCLITest(t *testing.T, dir, name, bundleID string) string {
	t.Helper()
	appDir := filepath.Join(dir, name)
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleExecutable</key>
	<string>AppBin</string>
</dict>
</plist>
`, bundleID)
	if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(macosDir, "AppBin"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	return appDir
}

type mockTap struct {
	stopped bool
}

func (m *mockTap) Stop() {
	m.stopped = true
}

type mockProbe struct{}

func (mockProbe) HitTest(_ policy.Point, _ uint64, _ darwin.InspectMode) darwin.AXObservation {
	return darwin.AXObservation{
		PID:          101,
		ProcessKind:  darwin.ProcessDock,
		IsDockFinder: true,
		Role:         "AXDockItem",
		Subrole:      "AXApplicationDockItem",
		URLKind:      darwin.URLFinderApp,
	}
}

// failingWarmer is a probe whose every warmup step fails, used to verify that startup is not affected.
type failingWarmer struct{ mockProbe }

func (mockProbe) Recheck(_ policy.Point, _ uint64, _ bool) darwin.AXObservation {
	return mockProbe{}.HitTest(policy.Point{}, 0, darwin.InspectDock)
}

func (mockProbe) EndGesture() {}

func (failingWarmer) WarmUp(_ darwin.InspectMode, _ uint64) darwin.AXWarmupReport {
	return darwin.AXWarmupReport{Steps: []darwin.WarmupStep{
		{Name: "dock_read", Status: -25211, ElapsedNS: 3_000_000},
		{Name: "dock_hit", Status: 2, ElapsedNS: 0},
	}}
}

// desktopProbe simulates a hit on a folder icon on the native desktop.
type desktopProbe struct{}

func (p desktopProbe) Recheck(_ policy.Point, _ uint64, _ bool) darwin.AXObservation {
	return p.HitTest(policy.Point{}, 0, darwin.InspectActive)
}

func (desktopProbe) EndGesture() {}

func (desktopProbe) HitTest(_ policy.Point, _ uint64, _ darwin.InspectMode) darwin.AXObservation {
	return darwin.AXObservation{
		PID:                  4321,
		ProcessKind:          darwin.ProcessFinder,
		FinderContainer:      darwin.ContainerDesktopIcon,
		Role:                 "AXImage",
		URLKind:              darwin.URLFile,
		FilePath:             "/Users/test/Desktop/Projects",
		ProcGen:              7,
		ElementHash:          9,
		HasSelectionInfo:     true,
		SelectionCount:       1,
		IsSingleSelectedSelf: true,
	}
}

// newTestEnv points both the run directory and the LaunchAgent plist to temporary locations under base, without
// touching the user's real directories.
func newTestEnv(t *testing.T, base string) (runtimeEnv, chan struct{}) {
	t.Helper()
	sigCh := make(chan struct{})
	mainStopCh := make(chan struct{})
	var stopOnce sync.Once

	env := runtimeEnv{
		runDir:              func() (string, error) { return filepath.Join(base, "run"), nil },
		agentPlistPath:      func() (string, error) { return filepath.Join(base, "LaunchAgents", "io.findrelay.agent.plist"), nil },
		selfExecutable:      func() (string, error) { return "/usr/local/bin/findrelay", nil },
		finderAppPath:       func() string { return "/System/Library/CoreServices/Finder.app" },
		inspectBundle:       darwin.InspectAppBundle,
		doubleClickInterval: func() uint64 { return 500_000_000 },
		sessionInfo: func() (service.SessionInfo, error) {
			return service.SessionInfo{ID: 100015, HasGraphicAccess: true}, nil
		},
		systemInfo: func() diagnostics.SystemVersion {
			return diagnostics.SystemVersion{ProductVersion: "15.2", BuildVersion: "24C101", Arch: "arm64"}
		},
		isAXTrusted:       func(_ bool) bool { return true },
		openAppAsync:      func(_ string, onComplete func(error)) { onComplete(nil) },
		openDirAsync:      func(_, _ string, onComplete func(error)) { onComplete(nil) },
		openDirViaCommand: func(_, _ string, onComplete func(error)) { onComplete(nil) },
		classifyDirectory: func(string) (darwin.DirStatus, uint64, uint64) {
			return darwin.DirEligible, 1, 2
		},
		revalidateDirectory: func(string, uint64, uint64) error { return nil },
		newProbe: func(_ string) (relay.HitProbe, func(), error) {
			return mockProbe{}, func() {}, nil
		},
		warmupAX: func(_ relay.HitProbe, _ darwin.InspectMode, _ *diagnostics.Recorder) {},
		startTap: func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
			return &mockTap{}, nil
		},
		prepareMainLoop: func() {},
		runMainLoop:     func() { <-mainStopCh },
		stopMainLoop:    func() { stopOnce.Do(func() { close(mainStopCh) }) },
		shutdownSignal:  func() (<-chan struct{}, func()) { return sigCh, func() {} },
		serviceMode:     func() bool { return false },
	}
	// The default stand-ins reject every launchctl call and every background log write: tests of service commands
	// must inject stand-ins explicitly and must never reach the real launchctl.
	env.launchctl = func(args ...string) (string, error) {
		t.Errorf("unexpected launchctl call: %v", args)
		return "", errors.New("unexpected launchctl call")
	}
	env.serviceLog = func() io.WriteCloser {
		t.Error("unexpected request for the service log writer")
		return nopWriteCloser{io.Discard}
	}
	return env, sigCh
}

// nopWriteCloser makes any io.Writer satisfy io.WriteCloser.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestRunVersion(t *testing.T) {
	version = "test"

	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("run(version) exit code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	if got, want := stdout.String(), "findrelay test\n"; got != want {
		t.Errorf("run(version) stdout = %q, want %q", got, want)
	}
}

func TestRunUsageError(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no arguments", args: nil},
		{name: "unknown command", args: []string{"nope"}},
		{name: "run invalid intercept mode", args: []string{"run", "--dock-intercept=middle"}},
		{name: "run invalid log level", args: []string{"run", "--app", "/Applications/Bloom.app", "--log-level=verbose"}},
		{name: "run open command without placeholder", args: []string{"run", "--app", "/Applications/Bloom.app", "--open-command", "open -a Bloom"}},
		{name: "run open command multi-line", args: []string{"run", "--app", "/Applications/Bloom.app", "--open-command", "echo $folder\nrm -rf /"}},
		{name: "doctor unexpected arguments", args: []string{"doctor", "extra"}},
	}
	base := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := newTestEnv(t, base)
			var stdout, stderr bytes.Buffer
			if code := runWithEnv(tt.args, &stdout, &stderr, env); code != exitUsage {
				t.Fatalf("run(%q) exit code = %d, want %d, stderr = %q", tt.args, code, exitUsage, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%q) stdout = %q, want empty", tt.args, stdout.String())
			}
		})
	}
}

// TestHelpWritesUsageToStdout checks that --help is a normal usage query: the usage is written to stdout and the exit
// code is 0.
func TestHelpWritesUsageToStdout(t *testing.T) {
	base := t.TempDir()
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"run", "--help"}, {"doctor", "-h"}} {
		env, _ := newTestEnv(t, base)
		var stdout, stderr bytes.Buffer
		if code := runWithEnv(args, &stdout, &stderr, env); code != exitOK {
			t.Fatalf("run(%q) exit code = %d, want %d, stderr = %q", args, code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Usage:") && !strings.Contains(stdout.String(), "Commands:") {
			t.Fatalf("run(%q) stdout must contain the usage, got %q", args, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("run(%q) stderr = %q, want empty", args, stderr.String())
		}
	}
}

func TestDoctorReportsAppServicePermissionsAndInstance(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	finderClone := makeBundleForCLITest(t, base, "FakeFinder.app", appbundle.FinderBundleID)
	env, _ := newTestEnv(t, base)
	var stdout, stderr bytes.Buffer

	// 1. No target application specified and no service installed: explain how to specify one, without an error.
	if code := runWithEnv([]string{"doctor"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("doctor bare code = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"[Target App]  not specified", "[Service]     not installed", "[Permissions] OK", "STOPPED in GUI session 100015"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}

	// 2. An explicitly specified valid application passes, and a Finder clone is rejected (exit code 3).
	stdout.Reset()
	if code := runWithEnv([]string{"doctor", "--app", validApp}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("doctor --app valid code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[Target App]  OK ("+validApp+", bundle=club.bloom.app)") {
		t.Errorf("doctor output missing target app line:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runWithEnv([]string{"doctor", "--app", finderClone}, &stdout, &stderr, env); code != exitTarget {
		t.Fatalf("doctor --app Finder clone code = %d, want %d", code, exitTarget)
	}
	if !strings.Contains(stdout.String(), "[Target App]  FAIL") {
		t.Errorf("doctor output must report the failure:\n%s", stdout.String())
	}

	// 3. Service installed: show its arguments and reuse the target application from the service arguments.
	plistPath := filepath.Join(base, "LaunchAgents", "io.findrelay.agent.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>io.findrelay.agent</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/findrelay</string><string>run</string><string>--app</string><string>` + validApp + `</string><string>--desktop</string></array>
</dict></plist>`
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	stdout.Reset()
	if code := runWithEnv([]string{"doctor"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("doctor with installed service code = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"[Service]     INSTALLED", "args: /usr/local/bin/findrelay run --app " + validApp + " --desktop", "[Target App]  OK (" + validApp} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}

	// 4. The target application in the service arguments is no longer valid: exit code 3.
	if err := os.RemoveAll(validApp); err != nil {
		t.Fatalf("remove valid app: %v", err)
	}
	stdout.Reset()
	if code := runWithEnv([]string{"doctor"}, &stdout, &stderr, env); code != exitTarget {
		t.Fatalf("doctor with stale service app code = %d, want %d", code, exitTarget)
	}
}

func TestRunExitCodesAndLifecycle(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	var stdout, stderr bytes.Buffer

	// 1. Active mode without --app returns exit code 3 and does not create a tap.
	tapCreated := false
	env, _ := newTestEnv(t, base)
	env.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
		tapCreated = true
		return &mockTap{}, nil
	}
	if code := runWithEnv([]string{"run"}, &stdout, &stderr, env); code != exitTarget {
		t.Fatalf("run without --app code = %d, want %d", code, exitTarget)
	}
	if tapCreated || !strings.Contains(stderr.String(), "--app") {
		t.Fatalf("expected no tap and an --app hint, tapCreated=%v stderr=%q", tapCreated, stderr.String())
	}

	// 2. Observe mode needs no target application and exits cleanly (exit code 0).
	envObs, sigObs := newTestEnv(t, base)
	close(sigObs)
	stdout.Reset()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--observe"}, &stdout, &stderr, envObs); code != exitOK {
		t.Fatalf("run --observe code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}

	// 3. With both entries disabled, exit 0 immediately without creating a tap.
	tapCreated = false
	stdout.Reset()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", validApp, "--dock=false"}, &stdout, &stderr, env); code != exitOK || tapCreated {
		t.Fatalf("run with all entries disabled code=%d tapCreated=%v, want 0 and false", code, tapCreated)
	}

	// 4. Invalid target application -> exit code 3.
	stdout.Reset()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", filepath.Join(base, "Missing.app")}, &stdout, &stderr, env); code != exitTarget {
		t.Fatalf("run with missing app code = %d, want %d", code, exitTarget)
	}

	// 5. Accessibility not granted -> exit code 4.
	envNoAX, _ := newTestEnv(t, base)
	envNoAX.isAXTrusted = func(_ bool) bool { return false }
	stdout.Reset()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", validApp}, &stdout, &stderr, envNoAX); code != exitPermission {
		t.Fatalf("run without AX trust code = %d, want %d", code, exitPermission)
	}

	// 6. EventTap creation fails -> exit code 4.
	envTapFail, _ := newTestEnv(t, base)
	envTapFail.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
		return nil, darwin.ErrEventTapCreate
	}
	if code := runWithEnv([]string{"run", "--app", validApp}, &stdout, &stderr, envTapFail); code != exitPermission {
		t.Fatalf("run tap fail code = %d, want %d", code, exitPermission)
	}

	// 7. Single-instance conflict in the same GUI session -> exit code 6.
	runDir := filepath.Join(base, "run")
	heldLock, err := service.AcquireInstanceLock(runDir, env.sessionInfo)
	if err != nil {
		t.Fatalf("AcquireInstanceLock: %v", err)
	}
	envConflict, _ := newTestEnv(t, base)
	if code := runWithEnv([]string{"run", "--app", validApp}, &stdout, &stderr, envConflict); code != exitRuntime {
		_ = heldLock.Release()
		t.Fatalf("run instance conflict code = %d, want %d", code, exitRuntime)
	}
	_ = heldLock.Release()

	// 8. A normal active run (testing --dock-intercept=down) returns 0 after it receives an exit signal.
	envActive, sigActive := newTestEnv(t, base)
	var openedApp string
	envActive.openAppAsync = func(appPath string, onComplete func(error)) {
		openedApp = appPath
		onComplete(nil)
	}
	envActive.startTap = func(_ bool, cb darwin.TapCallbacks) (tapHandle, error) {
		go func() {
			cb.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 10_000_000})
			cb.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 60_000_000})
			close(sigActive)
		}()
		return &mockTap{}, nil
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", validApp, "--debug", "--dock-intercept=down"}, &stdout, &stderr, envActive); code != exitOK {
		t.Fatalf("active run code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	if openedApp != validApp {
		t.Fatalf("openedApp = %q, want %q", openedApp, validApp)
	}
	// After the run, no state files are left behind, only the single-instance lock file.
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("read run dir: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lock") {
			t.Fatalf("unexpected file left in run dir: %q", entry.Name())
		}
	}
}

// TestRunUsesOpenCommandForDesktopDirectories checks that, when an open command is configured, directories redirected
// by a desktop double-click are handed to the target application through that command instead of the standard system
// open method.
func TestRunUsesOpenCommandForDesktopDirectories(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")

	envRun, sigCh := newTestEnv(t, base)
	var mu sync.Mutex
	var opened []string
	envRun.openDirViaCommand = func(template, dirPath string, onComplete func(error)) {
		mu.Lock()
		opened = append(opened, "command "+template+" -> "+dirPath)
		mu.Unlock()
		onComplete(nil)
	}
	envRun.openDirAsync = func(dirPath, appPath string, onComplete func(error)) {
		mu.Lock()
		opened = append(opened, "system "+appPath+" -> "+dirPath)
		mu.Unlock()
		onComplete(nil)
	}
	envRun.newProbe = func(string) (relay.HitProbe, func(), error) { return desktopProbe{}, func() {}, nil }
	envRun.startTap = func(_ bool, cb darwin.TapCallbacks) (tapHandle, error) {
		go func() {
			pt := policy.Point{X: 500, Y: 1040}
			now := darwin.MonotonicNowNS()
			click := func(kind policy.EventKind, count int, offset uint64) darwin.TapDisposition {
				return cb.OnMouseEvent(policy.Event{Kind: kind, Point: pt, ClickCount: count, TimeNS: now + offset})
			}
			if click(policy.EventDown, 1, 0) != darwin.TapPass || click(policy.EventUp, 1, uint64(50*time.Millisecond)) != darwin.TapPass {
				t.Errorf("first click must pass through")
			}
			// The worker classifies the first click asynchronously; the classifier function returns synchronously, so
			// this only waits for it to be scheduled.
			time.Sleep(100 * time.Millisecond)
			click(policy.EventDown, 2, uint64(200*time.Millisecond))
			click(policy.EventUp, 2, uint64(250*time.Millisecond))
			close(sigCh)
		}()
		return &mockTap{}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := runWithEnv([]string{"run", "--app", validApp, "--dock=false", "--desktop", "--open-command", `open -a Bloom "$folder"`, "--debug"}, &stdout, &stderr, envRun); code != exitOK {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "open=command") {
		t.Fatalf("debug banner must announce command mode:\n%s", stderr.String())
	}
	mu.Lock()
	got := append([]string(nil), opened...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "command open -a Bloom \"$folder\" -> /Users/test/Desktop/Projects" {
		t.Fatalf("open calls = %q, want exactly one command invocation with the clicked directory", got)
	}
	if strings.Contains(stderr.String(), "/Users/test/Desktop") {
		t.Fatalf("debug logs leaked the directory path:\n%s", stderr.String())
	}
}

// TestRunWarmsUpAXBeforeCreatingTap checks that startup warmup runs before the event tap is created and picks the
// warmup path by entry.
func TestRunWarmsUpAXBeforeCreatingTap(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	cases := []struct {
		name     string
		args     []string
		wantMode darwin.InspectMode
	}{
		{name: "dock only", args: []string{"run", "--app", validApp}, wantMode: darwin.InspectDock},
		{name: "dock and desktop", args: []string{"run", "--app", validApp, "--desktop"}, wantMode: darwin.InspectActive},
		{name: "observe", args: []string{"run", "--observe"}, wantMode: darwin.InspectObserve},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var gotMode darwin.InspectMode
			env, sig := newTestEnv(t, base)
			env.warmupAX = func(_ relay.HitProbe, mode darwin.InspectMode, _ *diagnostics.Recorder) {
				calls = append(calls, "warmup")
				gotMode = mode
			}
			env.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
				calls = append(calls, "startTap")
				go func() { close(sig) }()
				return &mockTap{}, nil
			}
			var stdout, stderr bytes.Buffer
			if code := runWithEnv(tc.args, &stdout, &stderr, env); code != exitOK {
				t.Fatalf("run code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
			}
			if !slices.Equal(calls, []string{"warmup", "startTap"}) {
				t.Fatalf("call order = %v, want warmup before startTap", calls)
			}
			if gotMode != tc.wantMode {
				t.Fatalf("warmup mode = %v, want %v", gotMode, tc.wantMode)
			}
		})
	}
}

// TestRunContinuesWhenWarmupFails checks that a failure in any warmup step only logs one redacted line: startup still
// creates the tap, and the log contains only the step, the result code and the duration.
func TestRunContinuesWhenWarmupFails(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")
	tapCreated := false
	env, sig := newTestEnv(t, base)
	env.newProbe = func(string) (relay.HitProbe, func(), error) {
		return failingWarmer{}, func() {}, nil
	}
	env.warmupAX = relay.WarmUpAX
	env.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
		tapCreated = true
		go func() { close(sig) }()
		return &mockTap{}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := runWithEnv([]string{"run", "--app", validApp, "--debug"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("run code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	if !tapCreated {
		t.Fatal("startup must still create the event tap when warmup fails")
	}
	logs := stderr.String()
	for _, want := range []string{"state=warmup_incomplete", "dock_read=ax:-25211/3.0ms", "dock_hit=icon_not_found/0.0ms"} {
		if !strings.Contains(logs, want) {
			t.Errorf("warmup log missing %q:\n%s", want, logs)
		}
	}
	if strings.Count(logs, "warmup_incomplete") != 1 {
		t.Errorf("a failed warmup must be reported exactly once:\n%s", logs)
	}
	for _, secret := range []string{"/System/", "Finder.app", "Dock.app"} {
		if strings.Contains(logs, secret) {
			t.Errorf("warmup log leaked %q:\n%s", secret, logs)
		}
	}
}

// pointProbe hits the Dock Finder icon at one coordinate and a third-party window at another.
type pointProbe struct{}

func (p pointProbe) Recheck(pt policy.Point, deadlineNS uint64, _ bool) darwin.AXObservation {
	return p.HitTest(pt, deadlineNS, darwin.InspectDock)
}

func (pointProbe) EndGesture() {}

func (pointProbe) HitTest(pt policy.Point, _ uint64, _ darwin.InspectMode) darwin.AXObservation {
	if pt.X == 10 {
		return darwin.AXObservation{PID: 101, ProcessKind: darwin.ProcessDock, IsDockFinder: true, Role: "AXDockItem"}
	}
	return darwin.AXObservation{PID: 500, ProcessKind: darwin.ProcessOther}
}

// TestRunPrintsLatencySummaryOnExit checks that the process prints one callback latency summary line before it exits,
// counting ordinary and candidate events separately; a start whose tap creation failed has no events to summarize and
// prints nothing.
func TestRunPrintsLatencySummaryOnExit(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "Bloom.app", "club.bloom.app")

	env, sig := newTestEnv(t, base)
	env.newProbe = func(string) (relay.HitProbe, func(), error) { return pointProbe{}, func() {}, nil }
	env.startTap = func(_ bool, cb darwin.TapCallbacks) (tapHandle, error) {
		go func() {
			for _, x := range []float64{10, 20} {
				cb.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: x, Y: 1040}, ClickCount: 1, TimeNS: 10_000_000})
				cb.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: x, Y: 1040}, ClickCount: 1, TimeNS: 60_000_000})
			}
			close(sig)
		}()
		return &mockTap{}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := runWithEnv([]string{"run", "--app", validApp, "--debug"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("run code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	logs := stderr.String()
	for _, want := range []string{"callback latency: ordinary n=2 p50_us=", "| candidate n=2 p50_us="} {
		if !strings.Contains(logs, want) {
			t.Errorf("summary line missing %q:\n%s", want, logs)
		}
	}
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "findrelay run: callback latency: ") {
		t.Errorf("summary must be the last diagnostic line, got %q", last)
	}
	if strings.Contains(last, "1040") {
		t.Errorf("summary leaked a coordinate: %q", last)
	}

	envFail, _ := newTestEnv(t, base)
	envFail.startTap = func(_ bool, _ darwin.TapCallbacks) (tapHandle, error) {
		return nil, darwin.ErrEventTapCreate
	}
	stderr.Reset()
	if code := runWithEnv([]string{"run", "--app", validApp}, &stdout, &stderr, envFail); code != exitPermission {
		t.Fatalf("run tap fail code = %d, want %d", code, exitPermission)
	}
	if strings.Contains(stderr.String(), "callback latency") {
		t.Errorf("a startup that never created the tap must not print a latency summary:\n%s", stderr.String())
	}
}

func TestReadDarwinSystemVersion(t *testing.T) {
	sv := readDarwinSystemVersion()
	if sv.ProductVersion == "" || sv.BuildVersion == "" {
		t.Fatalf("expected non-empty macOS sysctl version info, got %+v", sv)
	}
}

func TestRealMacOSBundleValidationWithCFBundle(t *testing.T) {
	const safari = "/Applications/Safari.app"
	if _, err := os.Stat(safari); err != nil {
		t.Skip("Safari.app not present")
	}
	base := t.TempDir()
	env, _ := newTestEnv(t, base)
	env.finderAppPath = darwin.FinderApplicationPath

	var stdout, stderr bytes.Buffer
	if code := runWithEnv([]string{"doctor", "--app", safari}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("doctor --app Safari.app with CFBundle inspector failed: code=%d stderr=%q", code, stderr.String())
	}

	// The system's real Finder.app must be rejected.
	if code := runWithEnv([]string{"doctor", "--app", darwin.FinderApplicationPath()}, &stdout, &stderr, env); code != exitTarget {
		t.Fatalf("expected Finder.app to be rejected with exitTarget, got %d", code)
	}

	// If FileOS.app is installed on this machine, also verify that the CFBundle check passes.
	const fileOS = "/Applications/FileOS.app"
	if _, err := os.Stat(fileOS); err == nil {
		if code := runWithEnv([]string{"doctor", "--app", fileOS}, &stdout, &stderr, env); code != exitOK {
			t.Fatalf("doctor --app FileOS.app failed: code=%d stderr=%q", code, stderr.String())
		}
	}
}

type stopCallbackTap struct {
	cb darwin.TapCallbacks
}

func (s *stopCallbackTap) Stop() {
	// Reproduce the real-machine timing in which CGEventTapEnable(port, false) delivers TapDisabledByUserInput at the
	// moment of shutdown.
	if s.cb != nil {
		s.cb.OnTapDisabled(darwin.TapDisabledByUserInput, nil)
	}
}

func TestCtrlCDoesNotFailWithTapDisabledByUserInput(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "FileOS.app", "com.fileosapp.fileos")

	envRun, sigCh := newTestEnv(t, base)
	envRun.startTap = func(_ bool, cb darwin.TapCallbacks) (tapHandle, error) {
		close(sigCh)
		return &stopCallbackTap{cb: cb}, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithEnv([]string{"run", "--app", validApp, "--debug", "--dock-intercept=down"}, &stdout, &stderr, envRun)
	if code != exitOK {
		t.Fatalf("Ctrl+C exit code = %d, want %d; stderr:\n%s", code, exitOK, stderr.String())
	}
	if strings.Contains(stderr.String(), "tap_disabled_by_user_input") {
		t.Fatalf("unexpected tap_disabled_by_user_input error on Ctrl+C:\n%s", stderr.String())
	}
}

// TestRunFallsBackToFinderWhenTargetAppDisappears covers a target application that disappears after the startup
// check: each redirect is preceded only by a stat recheck, and when it fails the click goes back to Finder and the
// degradation is reported.
func TestRunFallsBackToFinderWhenTargetAppDisappears(t *testing.T) {
	base := t.TempDir()
	validApp := makeBundleForCLITest(t, base, "FileOS.app", "com.fileosapp.fileos")

	envRun, sigCh := newTestEnv(t, base)
	var mu sync.Mutex
	var opened []string
	envRun.openAppAsync = func(appPath string, onComplete func(error)) {
		mu.Lock()
		opened = append(opened, appPath)
		mu.Unlock()
		onComplete(nil)
	}
	envRun.startTap = func(_ bool, cb darwin.TapCallbacks) (tapHandle, error) {
		go func() {
			// The startup check has already passed; move the app bundle away before the click to simulate the target
			// being uninstalled while running.
			if err := os.RemoveAll(validApp); err != nil {
				t.Errorf("remove app: %v", err)
			}
			cb.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 10_000_000})
			cb.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 60_000_000})
			close(sigCh)
		}()
		return &mockTap{}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := runWithEnv([]string{"run", "--app", validApp, "--debug", "--dock-intercept=down"}, &stdout, &stderr, envRun); code != exitOK {
		t.Fatalf("run code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
	}
	mu.Lock()
	got := append([]string(nil), opened...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "/System/Library/CoreServices/Finder.app" {
		t.Fatalf("open calls = %q, want a single Finder compensation", got)
	}
	if !strings.Contains(stderr.String(), "degraded (target_app_invalidated)") {
		t.Fatalf("degraded notice missing:\n%s", stderr.String())
	}
}
