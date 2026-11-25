package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.findrelay.agent</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/findrelay</string>
		<string>run</string>
		<string>--app</string>
		<string>/Applications/文件 管理器.app</string>
		<string>--desktop</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/bin:/bin</string>
		<key>ProgramArguments</key>
		<array>
			<string>decoy</string>
		</array>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>/tmp</string>
</dict>
</plist>
`

func TestReadAgentArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.plist")
	if args, err := ReadAgentArguments(path); err != nil || args != nil {
		t.Fatalf("missing plist: args=%v err=%v, want nil, nil", args, err)
	}

	if err := os.WriteFile(path, []byte(samplePlist), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	args, err := ReadAgentArguments(path)
	if err != nil {
		t.Fatalf("ReadAgentArguments error = %v", err)
	}
	want := []string{"/usr/local/bin/findrelay", "run", "--app", "/Applications/文件 管理器.app", "--desktop"}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args = %q, want %q", args, want)
	}

	if got, ok := FlagValue(args, "app"); !ok || got != "/Applications/文件 管理器.app" {
		t.Fatalf("FlagValue(app) = %q, %v", got, ok)
	}
	if _, ok := FlagValue(args, "desktop"); !ok {
		t.Fatal("desktop switch must be detected")
	}
	if _, ok := FlagValue(args, "dock"); ok {
		t.Fatal("absent switch must not be detected")
	}
	if got, ok := FlagValue([]string{"run", "--dock=false"}, "dock"); !ok || got != "false" {
		t.Fatalf("--dock=false handling wrong: %q %v", got, ok)
	}

	if err := os.WriteFile(path, []byte("bplist00\x00\x01binary"), 0o644); err != nil {
		t.Fatalf("write binary plist: %v", err)
	}
	if _, err := ReadAgentArguments(path); !errors.Is(err, ErrAgentPlistUnreadable) {
		t.Fatalf("binary plist error = %v, want ErrAgentPlistUnreadable", err)
	}
	if err := os.WriteFile(path, []byte(`<plist version="1.0"><dict><key>Label</key><string>x</string></dict></plist>`), 0o644); err != nil {
		t.Fatalf("write plist without arguments: %v", err)
	}
	if _, err := ReadAgentArguments(path); !errors.Is(err, ErrAgentPlistUnreadable) {
		t.Fatalf("plist without ProgramArguments error = %v, want ErrAgentPlistUnreadable", err)
	}

	// Label is read separately: only the top-level Label counts, and uninstall uses it to confirm that the plist
	// belongs to this tool.
	if err := os.WriteFile(path, []byte(samplePlist), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	def, err := ReadAgentDefinition(path)
	if err != nil || def == nil || def.Label != AgentLabel {
		t.Fatalf("ReadAgentDefinition = (%+v, %v), want label %s", def, err, AgentLabel)
	}
	if def, err := ReadAgentDefinition(filepath.Join(filepath.Dir(path), "missing.plist")); err != nil || def != nil {
		t.Fatalf("ReadAgentDefinition on a missing plist = (%+v, %v), want (nil, nil)", def, err)
	}
	noLabel := `<plist version="1.0"><dict><key>ProgramArguments</key><array><string>x</string></array><key>Env</key><dict><key>Label</key><string>decoy</string></dict></dict></plist>`
	if err := os.WriteFile(path, []byte(noLabel), 0o644); err != nil {
		t.Fatalf("write plist without a top-level Label: %v", err)
	}
	if def, err := ReadAgentDefinition(path); err != nil || def == nil || def.Label != "" {
		t.Fatalf("nested Label must be ignored: def=%+v err=%v", def, err)
	}
}

func TestWriteAgentPlistRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "LaunchAgents")
	path := filepath.Join(dir, AgentLabel+".plist")
	program := []string{
		"/usr/local/bin/findrelay",
		"run",
		"--app",
		"/Applications/文件 \"ForkLift\" & <Co>.app",
		"--open-command", `open -a ForkLift "$folder" && echo done`,
	}

	if err := WriteAgentPlist(path, AgentLabel, program); err != nil {
		t.Fatalf("WriteAgentPlist: %v", err)
	}
	def, err := ReadAgentDefinition(path)
	if err != nil || def == nil || def.Label != AgentLabel || strings.Join(def.ProgramArguments, "\x00") != strings.Join(program, "\x00") {
		t.Fatalf("round trip = (%+v, %v), want label %s and the original arguments", def, err, AgentLabel)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode = %v, %v; want 0644", info, err)
	}

	// A repeated write overwrites with the new content and leaves no temporary file in the directory.
	shorter := program[:2]
	if err := WriteAgentPlist(path, AgentLabel, shorter); err != nil {
		t.Fatalf("rewrite plist: %v", err)
	}
	if def, err := ReadAgentDefinition(path); err != nil || strings.Join(def.ProgramArguments, " ") != strings.Join(shorter, " ") {
		t.Fatalf("rewritten definition = (%+v, %v), want %q", def, err, shorter)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != AgentLabel+".plist" {
		t.Fatalf("LaunchAgents entries = %v, %v; want only the plist", entries, err)
	}
}
