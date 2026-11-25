package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunOpenCommandInjectsFolderThroughEnvironment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "工作 目录 with spaces")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cases := []struct {
		name     string
		template func(out string) string
	}{
		{name: "quoted $folder", template: func(out string) string { return `printf '%s' "$folder" > "` + out + `"` }},
		{name: "braced ${folder}", template: func(out string) string { return `printf '%s' "${folder}" > "` + out + `"` }},
		{name: "through a pipe", template: func(out string) string { return `printf '%s\n' "$folder" | tr a-z A-Z > "` + out + `"` }},
	}
	// Subtest names contain $folder, which must not appear in output paths (the shell would expand it), so all output
	// goes under the parent test's temp directory.
	outDir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(outDir, fmt.Sprintf("out%d.txt", i))
			if err := runOpenCommand("/bin/sh", 5*time.Second, tc.template(out), dir); err != nil {
				t.Fatalf("runOpenCommand error = %v", err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read output: %v", err)
			}
			want := dir
			if tc.name == "through a pipe" {
				want = strings.ToUpper(dir)
			}
			if got := strings.TrimRight(string(got), "\n"); got != want {
				t.Fatalf("command received %q, want %q", got, want)
			}
		})
	}
}

// When the template has exited but a background child process is still alive, Wait must not be held up by the
// leftover output pipe, and must not report a successful open as a failure.
func TestRunOpenCommandReturnsOnceTemplateExits(t *testing.T) {
	start := time.Now()
	if err := runOpenCommand("/bin/sh", 5*time.Second, `sleep 5 & echo started`, t.TempDir()); err != nil {
		t.Fatalf("runOpenCommand with a background child error = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("runOpenCommand waited %v for a background child to exit", elapsed)
	}
}

func TestRunOpenCommandReportsFailureCategory(t *testing.T) {
	dir := t.TempDir()

	err := runOpenCommand("/bin/sh", time.Second, `echo "some /private/path in output" >&2; exit 3`, dir)
	if !errors.Is(err, ErrOpenCommandFailed) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exit failure error = %v, want ErrOpenCommandFailed without deadline", err)
	}
	if strings.Contains(err.Error(), "/private/path") || strings.Contains(err.Error(), dir) {
		t.Fatalf("error must not echo command output or the directory: %v", err)
	}

	err = runOpenCommand("/bin/sh", 100*time.Millisecond, `sleep 5 & wait`, dir)
	if !errors.Is(err, ErrOpenCommandFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want ErrOpenCommandFailed wrapping context.DeadlineExceeded", err)
	}

	err = runOpenCommand(filepath.Join(t.TempDir(), "missing-shell"), time.Second, `true`, dir)
	if !errors.Is(err, ErrOpenCommandFailed) {
		t.Fatalf("missing shell error = %v, want ErrOpenCommandFailed", err)
	}
}

func TestNormalizeOpenCommandRequiresUsablePlaceholder(t *testing.T) {
	for _, valid := range []string{`  open -a Bloom "$folder"  `, `script "${folder}"`, `ls $folder/`, `open "$folder" && echo done`, `open $folder; echo done`} {
		got, err := NormalizeOpenCommand(valid)
		if err != nil {
			t.Fatalf("NormalizeOpenCommand(%q) error = %v, want accepted", valid, err)
		}
		if got != strings.TrimSpace(valid) {
			t.Fatalf("NormalizeOpenCommand(%q) = %q, want trimmed input", valid, got)
		}
	}

	invalid := map[string]string{
		"empty":                              "",
		"no placeholder":                     `open -a Bloom`,
		"placeholder followed by identifier": `open -a Bloom "$foldery"`,
		"placeholder followed by underscore": `open "$folder_"`,
		"multi-line":                         "open \"$folder\"\nrm -rf /",
		"too long":                           "open " + strings.Repeat("a", MaxOpenCommandRunes),
		"contains NUL":                       "open \"$folder\"\x00",
	}
	for name, raw := range invalid {
		if _, err := NormalizeOpenCommand(raw); !errors.Is(err, ErrInvalidOpenCommand) {
			t.Errorf("NormalizeOpenCommand(%s) error = %v, want ErrInvalidOpenCommand", name, err)
		}
	}
}
