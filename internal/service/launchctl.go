package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// launchctlPath is the fixed location of the system launchctl.
const launchctlPath = "/bin/launchctl"

// LaunchctlTimeout bounds the duration of a single launchctl call. bootout itself returns immediately; waiting for
// the old instance to exit is handled by the Manager's bounded wait.
const LaunchctlTimeout = 30 * time.Second

// LaunchctlExitNotFound is the launchctl exit code when the domain has no such service (measured on macOS 27;
// launchctl(1) does not document it). The Manager tells "not loaded" apart from "query failed" only by the exit code
// and does not parse launchctl output.
const LaunchctlExitNotFound = 113

// LaunchctlRunner runs one launchctl subcommand and returns the combined output; a non-zero exit is returned as
// *LaunchctlError.
type LaunchctlRunner func(args ...string) (string, error)

// LaunchctlError means a launchctl call failed: Code is the exit code, or -1 when launchctl could not run (not found,
// timeout).
type LaunchctlError struct {
	Args   []string
	Code   int
	Output string
	Err    error
}

func (e *LaunchctlError) Error() string {
	return fmt.Sprintf("launchctl %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *LaunchctlError) Unwrap() error { return e.Err }

// Launchctl runs launchctl in the real environment.
func Launchctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), LaunchctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, launchctlPath, args...).CombinedOutput()
	if err == nil {
		return string(out), nil
	}
	callErr := err
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if ctx.Err() != nil {
			callErr = fmt.Errorf("timed out after %s", LaunchctlTimeout)
		}
	}
	return string(out), &LaunchctlError{Args: args, Code: code, Output: string(out), Err: callErr}
}

// launchctlExitCode returns the exit code in a launchctl call error, or -1 when the error is not a launchctl exit
// error.
func launchctlExitCode(err error) int {
	var lc *LaunchctlError
	if errors.As(err, &lc) {
		return lc.Code
	}
	return -1
}
