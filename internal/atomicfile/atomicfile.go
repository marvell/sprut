// Package atomicfile writes files so that a reader sees either the old file
// whole or the new one whole, never one half-written.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces the file at path with data, with mode perm, through a
// temporary file in the same directory renamed over it.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // fails once renamed, as it should
	err = tmp.Chmod(perm)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
