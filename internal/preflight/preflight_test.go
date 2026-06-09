package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSysctl(t *testing.T, base, iface, name, val string) {
	t.Helper()
	dir := filepath.Join(base, "sys", "net", "ipv6", "conf", iface)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0644); err != nil {
		t.Fatalf("write sysctl %s/%s: %v", iface, name, err)
	}
}

func TestCheckPassesWhenCorrect(t *testing.T) {
	base := t.TempDir()
	for _, iface := range []string{"all", "wan0", "lan0"} {
		writeSysctl(t, base, iface, "forwarding", "1")
	}
	writeSysctl(t, base, "wan0", "accept_ra", "2")

	c := &Checker{
		WANInterface:  "wan0",
		LANInterfaces: []string{"lan0"},
		ProcPath:      base,
	}
	c.Check() // should not panic or error
}

func TestCheckWithForwardingDisabled(t *testing.T) {
	base := t.TempDir()
	for _, iface := range []string{"all", "wan0"} {
		writeSysctl(t, base, iface, "forwarding", "0")
	}
	writeSysctl(t, base, "wan0", "accept_ra", "1")

	c := &Checker{
		WANInterface: "wan0",
		ProcPath:     base,
	}
	c.Check() // should log warnings without panicking
}

func TestCheckMissingSysctls(t *testing.T) {
	// No sysctl files written — all reads fail — should warn but not panic.
	c := &Checker{
		WANInterface:  "wan0",
		LANInterfaces: []string{"lan0"},
		ProcPath:      t.TempDir(),
	}
	c.Check()
}
