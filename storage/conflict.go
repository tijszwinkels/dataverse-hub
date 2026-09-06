package storage

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// PreserveConflict keeps a candidate outside the live index, independently of
// ordinary revision backups. Identical retries reuse a file; distinct edits
// cannot overwrite each other. The caller must validate untrusted input first.
func PreserveConflict(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256(data)))
	tmp, err := os.CreateTemp(dir, ".conflict-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Linking the complete temporary file publishes it atomically and exclusively.
	if err := os.Link(tmp.Name(), path); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		saved, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(saved, data) {
			return "", fmt.Errorf("conflict archive differs at %s", path)
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) PreserveConflict(data []byte) (string, error) {
	return PreserveConflict(filepath.Join(s.dir, "conflicts"), data)
}
