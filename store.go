package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store handles flat-file object storage, compatible with the dataverse filesystem spec.
type Store struct {
	dir string
}

// NewStore creates a store rooted at the given directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Read returns the raw bytes of the object with the given ref.
func (s *Store) Read(ref string) ([]byte, error) {
	path := filepath.Join(s.dir, ref+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not found
		}
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	return data, nil
}

// Write atomically writes an object file and sets its mtime.
func (s *Store) Write(ref string, data []byte, ts time.Time) error {
	target := filepath.Join(s.dir, ref+".json")

	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // cleanup on failure

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	// Set mtime to object timestamp
	if err := os.Chtimes(target, ts, ts); err != nil {
		return fmt.Errorf("set mtime: %w", err)
	}

	return nil
}

// Exists returns true if the object file exists.
func (s *Store) Exists(ref string) bool {
	path := filepath.Join(s.dir, ref+".json")
	_, err := os.Stat(path)
	return err == nil
}

// Scan returns all refs (filenames without .json) in the store directory.
func (s *Store) Scan() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("scan store: %w", err)
	}

	var refs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		refs = append(refs, strings.TrimSuffix(name, ".json"))
	}
	return refs, nil
}
