package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupMovesOldFile(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	ref := "pk.00000000-0000-0000-0000-000000000001"
	data := []byte(`{"test":"original"}`)
	ts := time.Now()

	// Write an object
	if err := store.Write(ref, data, ts); err != nil {
		t.Fatal(err)
	}

	// Backup revision 3
	if err := store.Backup(ref, 3); err != nil {
		t.Fatal(err)
	}

	// Original file should be gone (moved)
	if store.Exists(ref) {
		t.Error("original file should have been moved by backup")
	}

	// Backup file should exist with correct content
	bkPath := filepath.Join(dir, "bk", ref+".r3.json")
	got, err := os.ReadFile(bkPath)
	if err != nil {
		t.Fatalf("backup file not found: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("backup content mismatch: got %q, want %q", got, data)
	}
}

func TestBackupNoopWhenFileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Backup a non-existent ref should be a no-op
	if err := store.Backup("nonexistent.ref", 0); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}

	// No bk/ directory should be created
	if _, err := os.Stat(filepath.Join(dir, "bk")); !os.IsNotExist(err) {
		t.Error("bk/ directory should not be created when there's nothing to back up")
	}
}

func TestBackupDisabled(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	ref := "pk.00000000-0000-0000-0000-000000000002"
	data := []byte(`{"test":"data"}`)
	ts := time.Now()

	if err := store.Write(ref, data, ts); err != nil {
		t.Fatal(err)
	}

	// Backup should be a no-op when disabled
	if err := store.Backup(ref, 1); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Original file should still be there (not moved)
	if !store.Exists(ref) {
		t.Error("original file should still exist when backup is disabled")
	}

	// No bk/ directory
	if _, err := os.Stat(filepath.Join(dir, "bk")); !os.IsNotExist(err) {
		t.Error("bk/ directory should not be created when backup is disabled")
	}
}
