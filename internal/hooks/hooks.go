package hooks

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

const defaultTimeout = 30 * time.Second

// Runner executes hook scripts found in Dir in filename order.
// Scripts receive the prefix change details via environment variables.
type Runner struct {
	Dir     string        // directory of hook scripts (empty = disabled)
	Timeout time.Duration // per-script timeout; 0 uses defaultTimeout (30s)
}

// Run executes all executable files in Dir with the following env vars set:
//
//	ACTION      added | changed | removed
//	INTERFACE   upstream WAN interface name
//	NEW_PREFIX  new /64 prefix (empty string if removed)
//	OLD_PREFIX  previous /64 prefix (empty string if added)
//
// Non-zero exits are logged but do not stop execution of remaining scripts.
// Returns the number of scripts that exited non-zero or timed out.
func (r *Runner) Run(action, iface, newPrefix, oldPrefix string) int {
	if r.Dir == "" {
		return 0
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		slog.Warn("hooks: failed to read hooks dir", "dir", r.Dir, "err", err)
		return 0
	}

	var scripts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0111 == 0 {
			continue // not executable
		}
		scripts = append(scripts, filepath.Join(r.Dir, e.Name()))
	}
	sort.Strings(scripts)

	env := append(os.Environ(),
		"ACTION="+action,
		"INTERFACE="+iface,
		"NEW_PREFIX="+newPrefix,
		"OLD_PREFIX="+oldPrefix,
	)

	failed := 0
	for _, script := range scripts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, script)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			failed++
			slog.Warn("hooks: script failed", "script", script, "err", err, "output", string(out))
		} else {
			slog.Debug("hooks: script ok", "script", script)
		}
	}
	return failed
}
