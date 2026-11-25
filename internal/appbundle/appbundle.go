// Package appbundle validates the target .app bundle: the path is valid, it is executable, and it is neither Finder
// nor FindRelay itself.
package appbundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// FinderBundleID is the bundle identifier of the native macOS Finder.
	FinderBundleID = "com.apple.finder"

	// DefaultFinderAppPath is the system app bundle path of the native macOS Finder.
	DefaultFinderAppPath = "/System/Library/CoreServices/Finder.app"
)

// ErrInvalidApp indicates that the target app path or app bundle is invalid.
var ErrInvalidApp = errors.New("invalid target application")

// BundleInfo describes the core metadata of a parsed macOS .app bundle.
type BundleInfo struct {
	BundleID       string
	ExecutablePath string
}

// BundleInspector validates the given .app path and extracts its bundle metadata.
type BundleInspector func(appPath string) (BundleInfo, error)

// ValidateOptions is the context needed to validate the target app bundle; when SelfExecutable is empty, the
// comparison against FindRelay itself is skipped.
type ValidateOptions struct {
	Inspector      BundleInspector
	SelfExecutable string
	FinderAppPath  string
}

// NormalizeCLIAppPath converts a path passed on the CLI into a clean absolute path, without expanding ~ or
// environment variables.
func NormalizeCLIAppPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: application path cannot be empty", ErrInvalidApp)
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: application path contains NUL byte", ErrInvalidApp)
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		return "", fmt.Errorf("%w: '~' is not expanded; pass an absolute .app path", ErrInvalidApp)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("%w: resolve path: %v", ErrInvalidApp, err)
	}
	return filepath.Clean(abs), nil
}

// ValidateApplication validates that the target .app path is valid and executable, and is neither Finder nor
// FindRelay itself.
func ValidateApplication(appPath string, opts ValidateOptions) (BundleInfo, error) {
	if appPath == "" {
		return BundleInfo{}, fmt.Errorf("%w: application path is empty", ErrInvalidApp)
	}
	if strings.ContainsRune(appPath, 0) {
		return BundleInfo{}, fmt.Errorf("%w: application path contains NUL byte", ErrInvalidApp)
	}
	if !strings.HasPrefix(appPath, "/") {
		return BundleInfo{}, fmt.Errorf("%w: application path %q must be an absolute path", ErrInvalidApp, appPath)
	}

	cleaned := filepath.Clean(appPath)
	base := filepath.Base(cleaned)
	if len(base) <= 4 || !strings.EqualFold(filepath.Ext(base), ".app") {
		return BundleInfo{}, fmt.Errorf("%w: application path %q must end with .app", ErrInvalidApp, appPath)
	}

	if samePathOrTarget(cleaned, opts.FinderAppPath) {
		return BundleInfo{}, fmt.Errorf("%w: cannot redirect to Finder (%s)", ErrInvalidApp, cleaned)
	}

	fi, err := os.Stat(cleaned)
	if err != nil {
		return BundleInfo{}, fmt.Errorf("%w: stat application %q: %v", ErrInvalidApp, cleaned, err)
	}
	if !fi.IsDir() {
		return BundleInfo{}, fmt.Errorf("%w: application %q is not a bundle directory", ErrInvalidApp, cleaned)
	}

	info, err := opts.Inspector(cleaned)
	if err != nil {
		return BundleInfo{}, fmt.Errorf("%w: invalid .app bundle %q: %v", ErrInvalidApp, cleaned, err)
	}

	if strings.EqualFold(info.BundleID, FinderBundleID) {
		return BundleInfo{}, fmt.Errorf("%w: cannot redirect to Finder (%s)", ErrInvalidApp, FinderBundleID)
	}

	if info.ExecutablePath == "" {
		return BundleInfo{}, fmt.Errorf("%w: bundle %q has no executable", ErrInvalidApp, cleaned)
	}
	execStat, err := os.Stat(info.ExecutablePath)
	if err != nil {
		return BundleInfo{}, fmt.Errorf("%w: bundle executable %q: %v", ErrInvalidApp, info.ExecutablePath, err)
	}
	if !execStat.Mode().IsRegular() || execStat.Mode().Perm()&0o111 == 0 {
		return BundleInfo{}, fmt.Errorf("%w: bundle executable %q is not executable", ErrInvalidApp, info.ExecutablePath)
	}

	if opts.SelfExecutable != "" && samePathOrTarget(info.ExecutablePath, opts.SelfExecutable) {
		return BundleInfo{}, fmt.Errorf("%w: cannot redirect to FindRelay itself", ErrInvalidApp)
	}

	return info, nil
}

// Revalidate confirms at runtime that the target app bundle and its executable still exist and are executable;
// the full identity and path validation is done only at startup by ValidateApplication.
func Revalidate(appPath string, info BundleInfo) error {
	if fi, err := os.Stat(appPath); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: application bundle is no longer available", ErrInvalidApp)
	}
	exec, err := os.Stat(info.ExecutablePath)
	if err != nil || !exec.Mode().IsRegular() || exec.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: bundle executable is no longer available", ErrInvalidApp)
	}
	return nil
}

func samePathOrTarget(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if ca == cb {
		return true
	}
	ra, errA := filepath.EvalSymlinks(ca)
	rb, errB := filepath.EvalSymlinks(cb)
	if errA == nil && errB == nil && ra == rb {
		return true
	}
	return false
}
