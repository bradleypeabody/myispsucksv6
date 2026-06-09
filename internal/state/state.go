package state

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

// State persists the last-seen prefix per upstream interface across restarts.
type State struct {
	Prefixes map[string]string `json:"prefixes"`
}

// Load reads the state file at path. Returns an empty State if the file does
// not exist (normal on first boot).
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{Prefixes: make(map[string]string)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decoding state: %w", err)
	}
	if s.Prefixes == nil {
		s.Prefixes = make(map[string]string)
	}
	return &s, nil
}

// Save writes the state to path atomically via a temp-file rename.
func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	return os.Rename(tmp, path)
}

// Get returns the last-seen prefix for iface. Returns false if not set or unparseable.
func (s *State) Get(iface string) (netip.Prefix, bool) {
	raw, ok := s.Prefixes[iface]
	if !ok {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, false
	}
	return p, true
}

// Set updates the last-seen prefix for iface.
func (s *State) Set(iface string, p netip.Prefix) {
	s.Prefixes[iface] = p.String()
}

// Delete removes the entry for iface (e.g. on prefix removal).
func (s *State) Delete(iface string) {
	delete(s.Prefixes, iface)
}
