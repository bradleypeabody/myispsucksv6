package preflight

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Checker reads IPv6 forwarding and accept_ra sysctls at startup and logs a
// WARN for each misconfiguration it finds. It never writes any sysctl values.
type Checker struct {
	WANInterface  string
	LANInterfaces []string
	ProcPath      string // defaults to /proc; override for testing
}

// Check reads all relevant sysctls and logs warnings for any misconfigurations.
// All checks are best-effort — an unreadable sysctl is itself a warning.
func (c *Checker) Check() {
	root := c.ProcPath
	if root == "" {
		root = "/proc"
	}

	read := func(parts ...string) (string, error) {
		path := filepath.Join(append([]string{root, "sys"}, parts...)...)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}

	checkForwarding := func(iface string) {
		val, err := read("net", "ipv6", "conf", iface, "forwarding")
		if err != nil {
			slog.Warn("preflight: cannot read forwarding sysctl", "iface", iface, "err", err)
			return
		}
		if val != "1" {
			slog.Warn("preflight: IPv6 forwarding is disabled",
				"iface", iface,
				"current", val,
				"fix", fmt.Sprintf("sysctl net.ipv6.conf.%s.forwarding=1", iface),
			)
		}
	}

	checkForwarding("all")
	checkForwarding(c.WANInterface)
	for _, lan := range c.LANInterfaces {
		checkForwarding(lan)
	}
}
