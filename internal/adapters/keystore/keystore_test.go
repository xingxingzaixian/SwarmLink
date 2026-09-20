package keystore

import (
	"os"
	"testing"
)

func TestLoadOrCreateIsStableAndPersists(t *testing.T) {
	dir := t.TempDir()

	kp1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kp1.NodeID() != kp2.NodeID() {
		t.Fatal("node id not stable across loads")
	}

	fi, err := os.Stat(KeyPath(dir))
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("want 0600 got %#o", perm)
	}
}

func TestLoadOrCreateCreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("want 0700 got %#o", perm)
	}
}

func TestLoadOrCreateRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/identity", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("expected error for corrupt key")
	}
}
