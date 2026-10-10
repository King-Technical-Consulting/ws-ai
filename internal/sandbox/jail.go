package sandbox

import "os/exec"

// jailBinds are the paths a jailed command may write to; everything else
// in the container is mounted read-only.
var jailBinds = []string{Workspace, "/home/dev", "/tmp"}

// jailArgv wraps a bash command in bubblewrap: a read-only view of the
// container filesystem with only the writable paths in binds mounted
// read-write, a private PID, IPC and UTS namespace (so the command cannot
// see or signal the other processes in the container), no controlling
// terminal and a kill when the parent goes away. Network is left shared;
// the egress proxy and the internal Docker network are what restrict it.
func jailArgv(binds []string, dir, shell string) []string {
	a := []string{
		"bwrap", "--die-with-parent", "--new-session",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
	}
	for _, b := range binds {
		a = append(a, "--bind", b, b)
	}
	return append(a, "--chdir", dir, "bash", "-c", shell)
}

// jailAvailable reports whether bwrap is on PATH.
func jailAvailable() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}
