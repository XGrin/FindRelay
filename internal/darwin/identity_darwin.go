package darwin

import (
	"errors"
	"os"
	"syscall"
)

// ErrDirectoryIdentityChanged means the path no longer exists, is no longer a regular directory, or has been replaced
// by another object at the same path.
var ErrDirectoryIdentityChanged = errors.New("directory identity changed")

// RevalidateDirectory uses lstat to re-verify that the directory is still the same object recorded in the snapshot: it
// is currently a directory that is not a symbolic link, and both its device number and inode match the snapshot. A path
// that has disappeared or is no longer a directory (moved, deleted, replaced) also counts as an identity change; other
// errors, such as permission errors, are returned unchanged.
func RevalidateDirectory(path string, dev, ino uint64) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return ErrDirectoryIdentityChanged
		}
		return err
	}
	if !fi.IsDir() {
		return ErrDirectoryIdentityChanged
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrDirectoryIdentityChanged
	}
	if uint64(st.Dev) != dev || uint64(st.Ino) != ino {
		return ErrDirectoryIdentityChanged
	}
	return nil
}
