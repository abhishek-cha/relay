// Package fsutil holds the small filesystem primitives Relay's on-disk state
// depends on.
//
// Everything the daemon writes — registry records, installed binaries, the
// socket — is written to a temporary sibling first and renamed into place, so a
// reader never observes a partial file and an interrupted write leaves the
// previous state intact.
package fsutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFile writes data to path atomically.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	file, err := createTemp(path)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		abandon(file)
		return err
	}
	return commit(file, path, mode)
}

// CopyFile copies source to target atomically.
func CopyFile(source, target string, mode os.FileMode) (int64, error) {
	source_file, err := os.Open(source)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", source, err)
	}
	defer source_file.Close()

	file, err := createTemp(target)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(file, source_file)
	if err != nil {
		abandon(file)
		return 0, fmt.Errorf("copy %s: %w", source, err)
	}
	if err := commit(file, target, mode); err != nil {
		return 0, err
	}
	return written, nil
}

// ReadFile reads a whole file.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// createTemp makes a hidden temporary file next to the destination, so the
// final rename stays within one filesystem and is therefore atomic.
func createTemp(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	return file, nil
}

// abandon removes a temporary file that will not be committed.
func abandon(file *os.File) {
	name := file.Name()
	file.Close()
	os.Remove(name)
}

// commit flushes and renames a temporary file over its destination.
func commit(file *os.File, path string, mode os.FileMode) error {
	name := file.Name()
	if err := file.Sync(); err != nil {
		abandon(file)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
