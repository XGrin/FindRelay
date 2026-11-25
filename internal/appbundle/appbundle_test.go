package appbundle_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/darwin"
)

// createTestAppBundle writes a minimal working .app bundle (XML Info.plist + executable) under parentDir.
func createTestAppBundle(t *testing.T, parentDir, name, bundleID, execName string, executable bool) string {
	t.Helper()
	appDir := filepath.Join(parentDir, name)
	contentsDir := filepath.Join(appDir, "Contents")
	macosDir := filepath.Join(contentsDir, "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleExecutable</key>
	<string>%s</string>
</dict>
</plist>
`, bundleID, execName)
	if err := os.WriteFile(filepath.Join(contentsDir, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatalf("write Info.plist: %v", err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(macosDir, execName), []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return appDir
}

func appbundleTestOptions(selfExec string) appbundle.ValidateOptions {
	return appbundle.ValidateOptions{
		Inspector:      darwin.InspectAppBundle,
		SelfExecutable: selfExec,
		FinderAppPath:  appbundle.DefaultFinderAppPath,
	}
}

// TestRejectInvalidApps checks that a valid path is accepted, while a missing path, an invalid app bundle, and
// FindRelay itself are each rejected distinctly.
func TestRejectInvalidApps(t *testing.T) {
	base := t.TempDir()
	finderFake := createTestAppBundle(t, base, "CustomFinder.app", appbundle.FinderBundleID, "Finder", true)
	selfApp := createTestAppBundle(t, base, "FindRelay.app", "com.example.wrapper", "findrelay", true)
	selfExec := filepath.Join(selfApp, "Contents", "MacOS", "findrelay")
	nonExecApp := createTestAppBundle(t, base, "NoExec.app", "com.example.noexec", "NoExec", false)
	validApp := createTestAppBundle(t, base, "Valid.app", "com.example.valid", "Valid", true)

	opts := appbundleTestOptions(selfExec)

	invalidApps := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "tilde unexpanded", path: "~/Applications/Valid.app"},
		{name: "not .app suffix", path: base},
		{name: "nonexistent .app", path: filepath.Join(base, "Missing.app")},
		{name: "system Finder path", path: "/System/Library/CoreServices/Finder.app"},
		{name: "Finder bundle ID", path: finderFake},
		{name: "self executable", path: selfApp},
		{name: "non-executable binary", path: nonExecApp},
	}
	for _, tc := range invalidApps {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := appbundle.ValidateApplication(tc.path, opts); !errors.Is(err, appbundle.ErrInvalidApp) {
				t.Fatalf("ValidateApplication(%q) error = %v, want ErrInvalidApp", tc.path, err)
			}
		})
	}

	info, err := appbundle.ValidateApplication(validApp, opts)
	if err != nil {
		t.Fatalf("ValidateApplication(valid) error = %v", err)
	}
	if info.BundleID != "com.example.valid" || info.ExecutablePath != filepath.Join(validApp, "Contents", "MacOS", "Valid") {
		t.Fatalf("unexpected info = %+v", info)
	}
}

// An app bundle that lacks CFBundleExecutable must be judged invalid.
func TestValidateApplicationRequiresExecutableName(t *testing.T) {
	app := createTestAppBundle(t, t.TempDir(), "NoName.app", "com.example.noname", "App", true)
	plist := `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.noname</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatalf("write Info.plist: %v", err)
	}
	if _, err := appbundle.ValidateApplication(app, appbundleTestOptions("/usr/local/bin/findrelay")); !errors.Is(err, appbundle.ErrInvalidApp) {
		t.Fatalf("ValidateApplication(bundle without CFBundleExecutable) error = %v, want ErrInvalidApp", err)
	}
}

func TestSpacesChineseAndRelativePath(t *testing.T) {
	base := t.TempDir()
	appPath := createTestAppBundle(t, filepath.Join(base, "我的 应用 目录"), "文件 管理器.app", "club.bloom.manager", "Bloom", true)
	opts := appbundleTestOptions("/usr/local/bin/findrelay")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	relPath, err := filepath.Rel(wd, appPath)
	if err != nil {
		relPath = appPath
	}

	abs, err := appbundle.NormalizeCLIAppPath(relPath)
	if err != nil {
		t.Fatalf("NormalizeCLIAppPath(relative path with spaces & Chinese) error = %v", err)
	}
	if abs != appPath {
		t.Fatalf("normalized path = %q, want %q", abs, appPath)
	}
	info, err := appbundle.ValidateApplication(abs, opts)
	if err != nil {
		t.Fatalf("ValidateApplication error = %v", err)
	}
	if info.BundleID != "club.bloom.manager" {
		t.Fatalf("BundleID = %q", info.BundleID)
	}

	for _, bad := range []string{"", "~", "~/Applications/Bloom.app", "Bloom\x00.app"} {
		if _, err := appbundle.NormalizeCLIAppPath(bad); !errors.Is(err, appbundle.ErrInvalidApp) {
			t.Fatalf("NormalizeCLIAppPath(%q) error = %v, want ErrInvalidApp", bad, err)
		}
	}
}

// Runtime revalidation only confirms that the app bundle and executable are still present and that the executable
// is still executable.
func TestRevalidateRequiresPresentExecutable(t *testing.T) {
	base := t.TempDir()
	app := createTestAppBundle(t, base, "Bloom.app", "club.bloom.app", "Bloom", true)
	info, err := appbundle.ValidateApplication(app, appbundleTestOptions("/usr/local/bin/findrelay"))
	if err != nil {
		t.Fatalf("ValidateApplication error = %v", err)
	}
	if err := appbundle.Revalidate(app, info); err != nil {
		t.Fatalf("Revalidate(valid) error = %v", err)
	}

	exec := info.ExecutablePath
	if err := os.Chmod(exec, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := appbundle.Revalidate(app, info); !errors.Is(err, appbundle.ErrInvalidApp) {
		t.Fatalf("Revalidate(non-executable) error = %v, want ErrInvalidApp", err)
	}
	if err := os.Remove(exec); err != nil {
		t.Fatalf("remove executable: %v", err)
	}
	if err := appbundle.Revalidate(app, info); !errors.Is(err, appbundle.ErrInvalidApp) {
		t.Fatalf("Revalidate(missing executable) error = %v, want ErrInvalidApp", err)
	}
	if err := os.RemoveAll(app); err != nil {
		t.Fatalf("remove bundle: %v", err)
	}
	if err := appbundle.Revalidate(app, info); !errors.Is(err, appbundle.ErrInvalidApp) {
		t.Fatalf("Revalidate(missing bundle) error = %v, want ErrInvalidApp", err)
	}
}

// The CLI path is an external input boundary: any bytes must yield only an absolute path or an error, never a panic.
func FuzzNormalizeCLIAppPath(f *testing.F) {
	f.Add("/Applications/Bloom.app")
	f.Add("relative/Bloom.app")
	f.Add("~/Bloom.app")
	f.Add("\x00")
	f.Fuzz(func(t *testing.T, raw string) {
		abs, err := appbundle.NormalizeCLIAppPath(raw)
		if err != nil {
			if !errors.Is(err, appbundle.ErrInvalidApp) {
				t.Fatalf("error = %v, want ErrInvalidApp", err)
			}
			return
		}
		if !strings.HasPrefix(abs, "/") {
			t.Fatalf("normalized path %q is not absolute", abs)
		}
	})
}
