package state

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := &State{Prefixes: make(map[string]string)}
	pfx := netip.MustParsePrefix("2607:fb90:1234:5678::/64")
	s.Set("eth0", pfx)

	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := s2.Get("eth0")
	if !ok {
		t.Fatal("Get: not found after round-trip")
	}
	if got != pfx {
		t.Errorf("want %s, got %s", pfx, got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	s, err := Load("/nonexistent/path/state.json")
	if err != nil {
		t.Fatalf("want nil error for missing file, got: %v", err)
	}
	if s == nil || s.Prefixes == nil {
		t.Fatal("want empty State, got nil")
	}
}

func TestDeleteAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := &State{Prefixes: make(map[string]string)}
	s.Set("eth0", netip.MustParsePrefix("2607::/64"))
	s.Delete("eth0")

	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s2, _ := Load(path)
	if _, ok := s2.Get("eth0"); ok {
		t.Error("expected eth0 to be absent after Delete")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// Verify that no .tmp file is left behind after a successful save.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := &State{Prefixes: make(map[string]string)}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not exist after successful save")
	}
}
