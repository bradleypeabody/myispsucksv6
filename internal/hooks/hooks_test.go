package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, content string, executable bool) {
	t.Helper()
	perm := os.FileMode(0644)
	if executable {
		perm = 0755
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), perm); err != nil {
		t.Fatalf("write script %s: %v", name, err)
	}
}

func TestRunEmptyDir(t *testing.T) {
	r := &Runner{Dir: ""}
	if n := r.Run("added", "eth0", "2607::/64", ""); n != 0 {
		t.Errorf("want 0 failed for empty Dir, got %d", n)
	}
}

func TestRunMissingDir(t *testing.T) {
	r := &Runner{Dir: "/nonexistent/hooks.d"}
	if n := r.Run("added", "eth0", "2607::/64", ""); n != 0 {
		t.Errorf("want 0 failed for missing dir, got %d", n)
	}
}

func TestRunPassesEnvVars(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")

	writeScript(t, dir, "10-check.sh", fmt.Sprintf(`#!/bin/sh
printf "%%s\n%%s\n%%s\n%%s\n" "$ACTION" "$INTERFACE" "$NEW_PREFIX" "$OLD_PREFIX" >> %s
`, out), true)

	r := &Runner{Dir: dir}
	if n := r.Run("changed", "enp1s0", "2607:fb90:aaaa:bbbb::/64", "2607:fb90:1111:2222::/64"); n != 0 {
		t.Fatalf("want 0 failures, got %d", n)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	got := string(data)
	for _, want := range []string{"changed", "enp1s0", "2607:fb90:aaaa:bbbb::/64", "2607:fb90:1111:2222::/64"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in script output, got:\n%s", want, got)
		}
	}
}

func TestRunFailingScriptContinues(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ran.txt")

	writeScript(t, dir, "10-fail.sh", "#!/bin/sh\nexit 1\n", true)
	writeScript(t, dir, "20-ok.sh", "#!/bin/sh\ntouch "+out+"\n", true)

	r := &Runner{Dir: dir}
	n := r.Run("added", "eth0", "2607::/64", "")
	if n != 1 {
		t.Errorf("want 1 failure, got %d", n)
	}
	if _, err := os.Stat(out); os.IsNotExist(err) {
		t.Error("second script did not run (expected it to continue after first failure)")
	}
}

func TestRunSkipsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "README.txt", "not a script", false)

	r := &Runner{Dir: dir}
	if n := r.Run("added", "eth0", "2607::/64", ""); n != 0 {
		t.Errorf("want 0 failures (non-executable skipped), got %d", n)
	}
}

func TestRunSkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Dir: dir}
	if n := r.Run("added", "eth0", "2607::/64", ""); n != 0 {
		t.Errorf("want 0 failures (subdir skipped), got %d", n)
	}
}

func TestRunTimeout(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "10-slow.sh", "#!/bin/sh\nsleep 60\n", true)

	r := &Runner{Dir: dir, Timeout: 100 * time.Millisecond}
	start := time.Now()
	n := r.Run("added", "eth0", "2607::/64", "")
	elapsed := time.Since(start)

	if n != 1 {
		t.Errorf("want 1 timeout failure, got %d", n)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout did not fire: took %v", elapsed)
	}
}
