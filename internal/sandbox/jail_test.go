package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runJailed runs shell under the real bubblewrap with one writable dir.
func runJailed(t *testing.T, work, shell string) (string, error) {
	t.Helper()
	argv := jailArgv([]string{work}, work, shell)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

func TestJailArgv(t *testing.T) {
	a := jailArgv([]string{"/workspace", "/tmp"}, "/workspace/x", "ls")
	got := strings.Join(a, " ")
	for _, want := range []string{"--unshare-pid", "--ro-bind / /", "--bind /workspace /workspace", "--bind /tmp /tmp", "--chdir /workspace/x bash -c ls"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q lacks %q", got, want)
		}
	}
}

func TestJailEnforces(t *testing.T) {
	if !jailAvailable() {
		t.Skip("bwrap not installed")
	}
	work := t.TempDir()
	if out, err := runJailed(t, work, "true"); err != nil {
		t.Skipf("bwrap cannot run here (user namespaces?): %v %s", err, out)
	}
	if out, err := runJailed(t, work, "echo hi > f && cat f"); err != nil || !strings.Contains(out, "hi") {
		t.Fatalf("write to the workspace should work: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(work, "f")); err != nil {
		t.Fatalf("file not on the real workspace: %v", err)
	}
	if out, err := runJailed(t, work, "echo x > /etc/jail-test"); err == nil {
		t.Fatalf("write outside the binds succeeded: %q", out)
	}
	// A private PID namespace: the shell is PID 1 or close to it, and the
	// host's processes are invisible.
	out, err := runJailed(t, work, "echo $$; ls /proc | grep -c '^[0-9]'")
	if err != nil {
		t.Fatal(err, out)
	}
	f := strings.Fields(out)
	pid, _ := strconv.Atoi(f[0])
	procs, _ := strconv.Atoi(f[len(f)-1])
	if len(f) != 2 || pid > 10 || procs > 10 {
		t.Errorf("expected a tiny pid and a short process list, got %q", out)
	}
}
