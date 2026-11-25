package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// AgentLabel is the service label of the per-user LaunchAgent; the plist file name is the same.
const AgentLabel = "io.findrelay.agent"

// Environment variable the LaunchAgent sets for the background process: run uses it to determine that it was started
// by the login service and writes its diagnostics to the system unified log.
const (
	ServiceEnvironmentKey   = "FINDRELAY_SERVICE"
	ServiceEnvironmentValue = "1"
)

// ErrAgentPlistUnreadable means the installed LaunchAgent plist cannot be parsed.
var ErrAgentPlistUnreadable = errors.New("cannot read installed LaunchAgent plist")

// DefaultRunDir returns the per-user run directory, which holds the single-instance lock of the current GUI session
// and the service operations lock.
func DefaultRunDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "FindRelay", "run"), nil
}

// DefaultAgentPlistPath returns the standard location of the user's LaunchAgent plist.
func DefaultAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", AgentLabel+".plist"), nil
}

// AgentDefinition is the definition this tool writes into the installed LaunchAgent.
type AgentDefinition struct {
	Label            string
	ProgramArguments []string
}

// ReadAgentDefinition reads Label and ProgramArguments of the installed LaunchAgent; it returns (nil, nil) when the
// plist does not exist.
func ReadAgentDefinition(plistPath string) (*AgentDefinition, error) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrAgentPlistUnreadable, err)
	}
	label, args, hasArguments, err := parseAgentDefinition(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAgentPlistUnreadable, err)
	}
	if !hasArguments || len(args) == 0 {
		return nil, fmt.Errorf("%w: ProgramArguments is missing or empty", ErrAgentPlistUnreadable)
	}
	return &AgentDefinition{Label: label, ProgramArguments: args}, nil
}

// ReadAgentArguments reads ProgramArguments of the installed LaunchAgent; it returns (nil, nil) when the plist does
// not exist.
func ReadAgentArguments(plistPath string) ([]string, error) {
	def, err := ReadAgentDefinition(plistPath)
	if err != nil || def == nil {
		return nil, err
	}
	return def.ProgramArguments, nil
}

// parseAgentDefinition extracts the top-level keys Label and ProgramArguments; values of other keys are skipped as a
// whole, and keys with the same names inside nested structures are ignored. hasArguments reports whether the plist
// contains ProgramArguments. Only XML plists are supported, which is the format this tool writes.
func parseAgentDefinition(data []byte) (label string, args []string, hasArguments bool, err error) {
	if !bytes.Contains(data, []byte("<plist")) {
		return "", nil, false, errors.New("unsupported plist format (expected XML)")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		key     string // top-level key just read; cleared once its value is consumed
		inArray bool   // reading the string array of ProgramArguments
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, false, err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			switch tok.Name.Local {
			case "key":
				if err := dec.DecodeElement(&key, &tok); err != nil {
					return "", nil, false, err
				}
				if key == "" {
					return "", nil, false, errors.New("empty plist key")
				}
			case "string":
				var value string
				if err := dec.DecodeElement(&value, &tok); err != nil {
					return "", nil, false, err
				}
				switch {
				case inArray:
					args = append(args, value)
				case key == "Label":
					label = value
				}
				if !inArray {
					key = ""
				}
			case "array":
				if key == "ProgramArguments" {
					inArray, hasArguments = true, true
					key = ""
					continue
				}
				if err := skipValue(dec, &key); err != nil {
					return "", nil, false, err
				}
			default:
				if err := skipValue(dec, &key); err != nil {
					return "", nil, false, err
				}
			}
		case xml.EndElement:
			if tok.Name.Local == "array" {
				inArray = false
			}
		}
	}
	return label, args, hasArguments, nil
}

// skipValue skips the value of the top-level key just read; before any key has been read, the elements met are the
// plist and dict containers, which are left as they are.
func skipValue(dec *xml.Decoder, key *string) error {
	if *key == "" {
		return nil
	}
	if err := dec.Skip(); err != nil {
		return err
	}
	*key = ""
	return nil
}

// WriteAgentPlist writes the LaunchAgent plist atomically: it first writes a temporary file in the same directory and
// then renames it over the target, so launchd never reads a half-written file.
func WriteAgentPlist(plistPath, label string, programArguments []string) error {
	dir := filepath.Dir(plistPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+AgentLabel+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary plist: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	_, writeErr := tmp.Write(renderAgentPlist(label, programArguments))
	if writeErr == nil {
		writeErr = tmp.Chmod(0o644)
	}
	if writeErr == nil {
		writeErr = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if writeErr == nil {
		writeErr = os.Rename(tmpPath, plistPath)
	}
	if writeErr != nil {
		return fmt.Errorf("write LaunchAgent plist: %w", writeErr)
	}
	return nil
}

// renderAgentPlist renders the LaunchAgent plist. RunAtLoad is on: once the service is allowed to launch at login,
// launchd loads it at login and starts the agent. KeepAlive is off, so the user diagnoses and recovers from abnormal
// exits. launchd discards the stdout and stderr of the background process.
func renderAgentPlist(label string, programArguments []string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	writeString := func(indent, key, value string) {
		b.WriteString(indent)
		b.WriteString("<key>")
		b.WriteString(key)
		b.WriteString("</key>\n")
		b.WriteString(indent)
		b.WriteString("<string>")
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString("</string>\n")
	}
	writeString("\t", "Label", label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range programArguments {
		b.WriteString("\t\t<string>")
		_ = xml.EscapeText(&b, []byte(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	writeString("\t\t", ServiceEnvironmentKey, ServiceEnvironmentValue)
	b.WriteString("\t</dict>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<false/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// FlagValue takes the value of a flag from an argument list, supporting both the `--name value` and `--name=value`
// forms.
func FlagValue(args []string, name string) (string, bool) {
	prefix := "--" + name + "="
	for i, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
		if arg == "--"+name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}
