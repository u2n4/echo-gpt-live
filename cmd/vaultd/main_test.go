package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendConfinedToVault(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if _, err := appendNote(root, "..", "test"); err == nil {
		t.Fatal("parent inbox accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	name := "voice-" + time.Now().Format("2006-01-02") + ".md"
	target := filepath.Join(outside, "untouched.md")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "inbox", name)); err != nil {
		t.Skip("symlink creation unavailable")
	}
	if _, err := appendNote(root, "inbox", "must not escape"); err == nil {
		t.Fatal("external symlink accepted")
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "original" {
		t.Fatalf("outside note changed: %q %v", b, err)
	}
}
