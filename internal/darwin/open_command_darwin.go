package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrOpenCommandFailed means the open command did not complete successfully: it exited with a non-zero status, could
// not start, or timed out.
// On timeout the error chain also contains context.DeadlineExceeded.
var ErrOpenCommandFailed = errors.New("open command failed")

// ErrInvalidOpenCommand means the open command template is invalid.
var ErrInvalidOpenCommand = errors.New("invalid open command")

// MaxOpenCommandRunes is the maximum number of characters in an open command template.
const MaxOpenCommandRunes = 500

// referencesFolder reports whether the template references the $folder or ${folder} placeholder; $folder must not be
// followed by an identifier character, so a typo such as $foldery is not a reference.
func referencesFolder(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '$' {
			continue
		}
		switch rest := cmd[i:]; {
		case strings.HasPrefix(rest, "${folder}"):
			return true
		case strings.HasPrefix(rest, "$folder"):
			rest = rest[len("$folder"):]
			if rest == "" || !isNameByte(rest[0]) {
				return true
			}
		}
	}
	return false
}

func isNameByte(b byte) bool {
	return b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// NormalizeOpenCommand validates and normalizes an open command template: it trims leading and trailing whitespace, and
// the result must be non-empty, within the length limit, a single line, free of NUL, and must reference the $folder (or
// ${folder}) placeholder; otherwise the directory cannot be passed to the target app.
func NormalizeOpenCommand(raw string) (string, error) {
	cmd := strings.TrimSpace(raw)
	if cmd == "" {
		return "", fmt.Errorf("%w: open command cannot be empty", ErrInvalidOpenCommand)
	}
	if len([]rune(cmd)) > MaxOpenCommandRunes {
		return "", fmt.Errorf("%w: open command exceeds %d characters", ErrInvalidOpenCommand, MaxOpenCommandRunes)
	}
	if strings.ContainsRune(cmd, 0) || strings.ContainsAny(cmd, "\r\n") {
		return "", fmt.Errorf("%w: open command must be a single line without control characters", ErrInvalidOpenCommand)
	}
	if !referencesFolder(cmd) {
		return "", fmt.Errorf("%w: open command must reference the folder with $folder", ErrInvalidOpenCommand)
	}
	return cmd, nil
}

const (
	// openCommandShell interprets the command template given by the user.
	openCommandShell = "/bin/sh"
	// openCommandTimeout covers the target app's cold start and UI automation scripts, and is far longer than the
	// executor's completion callback deadline, so a late result can still reclaim the executor slot.
	openCommandTimeout = 30 * time.Second
)

// CommandDirectoryOpener passes a directory to the target app using the command template given by the user.
type CommandDirectoryOpener func(commandTemplate, dirPath string, onComplete func(error))

// OpenDirectoryViaCommand runs the open command template asynchronously: it runs under /bin/sh -c, the directory path
// is injected through the environment variable folder, and the template references it as "$folder". Command output may
// contain paths, so it is always discarded; only the exit status decides the result.
func OpenDirectoryViaCommand(commandTemplate, dirPath string, onComplete func(error)) {
	go onComplete(runOpenCommand(openCommandShell, openCommandTimeout, commandTemplate, dirPath))
}

// runOpenCommand runs the open command synchronously and waits at most timeout.
func runOpenCommand(shell string, timeout time.Duration, commandTemplate, dirPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-c", commandTemplate)
	// The path is passed only through the environment and is never spliced into the script text; the quoting in the
	// template decides word splitting.
	cmd.Env = append(os.Environ(), "folder="+dirPath)
	// Standard streams stay nil: the child is connected directly to /dev/null, output is always discarded, and
	// background processes spawned by the template do not hold a pipe; otherwise Wait would wait until WaitDelay
	// expires and misreport a successful open as a failure.
	// On timeout or cancellation the whole process group is killed, so child processes spawned by the script (such as
	// UI automation) do not keep running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", ErrOpenCommandFailed, context.DeadlineExceeded)
	default:
		return fmt.Errorf("%w: %w", ErrOpenCommandFailed, err)
	}
}
