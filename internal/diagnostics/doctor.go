package diagnostics

import (
	"fmt"
	"io"
	"strings"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/service"
)

// SystemVersion describes the current macOS version information.
type SystemVersion struct {
	ProductVersion string
	BuildVersion   string
	Arch           string
}

// DoctorDeps holds the system, target-app and service query dependencies that doctor diagnostics need; callers must
// provide all of them.
type DoctorDeps struct {
	Version string
	// AppPath is the target app to validate; empty means none was specified.
	AppPath string
	// ValidateApp validates the target app bundle.
	ValidateApp func(appPath string) (appbundle.BundleInfo, error)
	// AgentPlist and AgentArgs describe the installed user LaunchAgent; a nil AgentArgs means it is not installed.
	AgentPlist string
	AgentArgs  []string
	// RunDir is the directory that holds the single-instance lock.
	RunDir       string
	SystemInfo   func() SystemVersion
	SessionInfo  service.SessionProvider
	IsAXTrusted  func() bool
	ProbeRunning func(runDir string, sessionID uint32) (bool, error)
}

// DoctorResult is the structured conclusion of the doctor checks.
type DoctorResult struct {
	AppSpecified   bool
	AppValid       bool
	AgentInstalled bool
	SessionValid   bool
	AXTrusted      bool
	AgentRunning   bool
}

// RunDoctor checks, in order, the system version, GUI session, target app, installed service, Accessibility
// preflight for the current process, and instance state of the current session.
// When the target app is invalid, it still prints the remaining items and finally returns that error so the caller
// can decide the exit code.
func RunDoctor(w io.Writer, deps DoctorDeps) (DoctorResult, error) {
	var res DoctorResult

	fmt.Fprintf(w, "FindRelay Doctor (%s)\n", deps.Version)
	fmt.Fprintln(w, "----------------------------------------")

	// 1. System version and architecture
	sys := deps.SystemInfo()
	fmt.Fprintf(w, "[System]      macOS %s (build %s) %s\n",
		nonEmpty(sys.ProductVersion, "unknown"),
		nonEmpty(sys.BuildVersion, "unknown"),
		sys.Arch,
	)

	// 2. GUI session identity
	var sessionID uint32
	sess, err := deps.SessionInfo()
	switch {
	case err != nil:
		fmt.Fprintf(w, "[Session]     FAIL (%v)\n", err)
	case sess.ID == 0 || !sess.HasGraphicAccess:
		fmt.Fprintf(w, "[Session]     WARN (id=%d graphic_access=%v; active agent requires GUI login session)\n", sess.ID, sess.HasGraphicAccess)
	default:
		res.SessionValid = true
		sessionID = sess.ID
		fmt.Fprintf(w, "[Session]     OK (id=%d graphic_access=true)\n", sess.ID)
	}

	// 3. Target app
	var appErr error
	res.AppSpecified = deps.AppPath != ""
	if !res.AppSpecified {
		fmt.Fprintln(w, "[Target App]  not specified (findrelay run --app \"/Applications/<App>.app\")")
	} else {
		info, err := deps.ValidateApp(deps.AppPath)
		if err != nil {
			appErr = err
			fmt.Fprintf(w, "[Target App]  FAIL (%v)\n", err)
		} else {
			res.AppValid = true
			fmt.Fprintf(w, "[Target App]  OK (%s, bundle=%s)\n", deps.AppPath, nonEmpty(info.BundleID, "none"))
		}
	}

	// 4. Installed user LaunchAgent
	if deps.AgentArgs == nil {
		fmt.Fprintln(w, "[Service]     not installed")
	} else {
		res.AgentInstalled = true
		fmt.Fprintf(w, "[Service]     INSTALLED (%s)\n", deps.AgentPlist)
		fmt.Fprintf(w, "              args: %s\n", strings.Join(deps.AgentArgs, " "))
	}

	// 5. Accessibility preflight for the current process (stating explicitly that it only represents the process
	// started from the current terminal)
	res.AXTrusted = deps.IsAXTrusted()
	if res.AXTrusted {
		fmt.Fprintln(w, "[Permissions] OK (Accessibility trusted for current caller process)")
	} else {
		fmt.Fprintln(w, "[Permissions] NOT GRANTED for current caller process (grant in System Settings > Privacy & Security > Accessibility; Input Monitoring may also be required if event tap creation fails)")
	}
	fmt.Fprintln(w, "              Note: terminal precheck reflects the terminal process; background service authorization depends on the agent binary's own tap/AX status.")

	// 6. Instance run state of the current session
	if sessionID != 0 {
		running, err := deps.ProbeRunning(deps.RunDir, sessionID)
		switch {
		case err != nil:
			fmt.Fprintf(w, "[Instance]    WARN (lock probe failed: %v)\n", err)
		case running:
			res.AgentRunning = true
			fmt.Fprintf(w, "[Instance]    RUNNING in GUI session %d\n", sessionID)
		default:
			fmt.Fprintf(w, "[Instance]    STOPPED in GUI session %d\n", sessionID)
		}
	}

	return res, appErr
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
