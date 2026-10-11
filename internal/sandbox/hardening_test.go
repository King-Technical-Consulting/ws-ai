package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/jking323/ws/internal/store"
)

func TestContainerSpecHardened(t *testing.T) {
	m := &Manager{cfg: Config{Image: "img", MemoryMB: 1024, CPUs: 1, Pids: 64}}
	_, hc, _ := m.containerSpec(nil)
	if !hc.ReadonlyRootfs {
		t.Error("root filesystem must be read-only")
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || len(hc.CapAdd) != 0 {
		t.Errorf("capabilities: drop %v add %v, want drop ALL and none added", hc.CapDrop, hc.CapAdd)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges:true" {
		t.Errorf("security opt %v", hc.SecurityOpt)
	}
	if hc.Privileged || len(hc.Devices) != 0 {
		t.Errorf("privileged %v devices %v", hc.Privileged, hc.Devices)
	}
	if hc.Memory != 1024<<20 || hc.PidsLimit == nil || *hc.PidsLimit != 64 {
		t.Errorf("limits: memory %d pids %v", hc.Memory, hc.PidsLimit)
	}
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Error("/tmp must be a tmpfs")
	}
	// The home directory is owned by the dev user (uid 1000): over the
	// image's root-owned directory a bare tmpfs is unwritable to the user
	// commands run as (found placing a skill folder, board 4d96).
	home := hc.Tmpfs["/home/dev"]
	for _, want := range []string{"uid=1000", "gid=1000", "mode=0755"} {
		if !strings.Contains(home, want) {
			t.Errorf("/home/dev tmpfs options %q lack %s", home, want)
		}
	}
}

// TestHomeIsWritableByTheDevUser creates the directory a placed skill needs
// under the home directory, as the user commands run as. It needs a Docker
// daemon and WS_TEST_SANDBOX_IMAGE.
func TestHomeIsWritableByTheDevUser(t *testing.T) {
	out := runInSandbox(t, false, "mkdir -p /home/dev/.ws-skills/x/scripts && echo placed")
	if !strings.Contains(out, "placed") {
		t.Fatalf("mkdir under /home/dev failed: %q", out)
	}
}

func TestContainerSpecHardeningOff(t *testing.T) {
	m := &Manager{cfg: Config{Image: "img", MemoryMB: 1024, CPUs: 1, Pids: 64, HardeningOff: true}}
	_, hc, _ := m.containerSpec(nil)
	if hc.ReadonlyRootfs || len(hc.CapDrop) != 0 || len(hc.SecurityOpt) != 0 {
		t.Errorf("off must drop the flags: ro %v capdrop %v secopt %v", hc.ReadonlyRootfs, hc.CapDrop, hc.SecurityOpt)
	}
	if hc.Privileged || len(hc.Devices) != 0 {
		t.Error("off must never mean privileged or host devices")
	}
	if hc.Memory != 1024<<20 || hc.PidsLimit == nil || *hc.PidsLimit != 64 {
		t.Errorf("limits stay with hardening off: memory %d pids %v", hc.Memory, hc.PidsLimit)
	}
}

// runInSandbox starts a container from the sandbox spec with cmd and returns
// its output. WS_TEST_SANDBOX_IMAGE names an image with a POSIX sh (the
// sandbox image); the test skips without it or without a Docker daemon.
func runInSandbox(t *testing.T, off bool, shell string) string {
	t.Helper()
	image := os.Getenv("WS_TEST_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("WS_TEST_SANDBOX_IMAGE not set")
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker daemon: %v", err)
	}
	m := &Manager{cfg: Config{Image: image, MemoryMB: 512, CPUs: 1, Pids: 128, HardeningOff: off}, runtime: "runc"}
	cfg, hc, _ := m.containerSpec(nil)
	cfg.Cmd = []string{"sh", "-c", shell}
	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hc})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	w := cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{})
	select {
	case <-w.Result:
	case err := <-w.Error:
		t.Fatalf("wait: %v", err)
	}
	logs, err := cli.ContainerLogs(ctx, res.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer logs.Close()
	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil && err != io.EOF {
		t.Fatalf("logs read: %v", err)
	}
	return out.String()
}

const inspect = `grep -E '^(NoNewPrivs|CapEff|CapBnd):' /proc/self/status; ` +
	`awk '$2=="/"{print "root", $4}' /proc/mounts | head -1`

// TestSandboxFlagsFromInside is the check the plan asks for: the flags as the
// process in the container sees them, not as the spec states them.
func TestSandboxFlagsFromInside(t *testing.T) {
	out := runInSandbox(t, false, inspect)
	for _, want := range []string{"NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("hardened sandbox lacks %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "root ro,") {
		t.Errorf("root should be mounted read-only:\n%s", out)
	}
}

func TestSandboxHardeningOffFromInside(t *testing.T) {
	out := runInSandbox(t, true, inspect)
	if strings.Contains(out, "NoNewPrivs:\t1") || strings.Contains(out, "CapBnd:\t0000000000000000") {
		t.Errorf("hardening off should leave Docker's defaults:\n%s", out)
	}
	if strings.Contains(out, "root ro,") {
		t.Errorf("root should be writable with hardening off:\n%s", out)
	}
}

// nopDB stands in for Postgres where a test only needs the touch to succeed.
type nopDB struct{}

func (nopDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (nopDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("nopDB: no query")
}
func (nopDB) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

// TestWriteFileIntoTmpfs writes where a placed skill goes: under /home/dev,
// a tmpfs, which Docker's archive API cannot write into (board 088b). It
// needs a Docker daemon and WS_TEST_SANDBOX_IMAGE.
func TestWriteFileIntoTmpfs(t *testing.T) {
	image := os.Getenv("WS_TEST_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("WS_TEST_SANDBOX_IMAGE not set")
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker daemon: %v", err)
	}
	m := &Manager{cli: cli, db: &store.DB{Queries: store.New(nopDB{})}, cfg: Config{Image: image, MemoryMB: 512, CPUs: 1, Pids: 128}, runtime: "runc"}
	cfg, hc, _ := m.containerSpec(nil)
	cfg.Cmd = []string{"sleep", "120"}
	hc.Tmpfs[Workspace] = "rw,size=16m,uid=1000,gid=1000"
	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hc})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	sb := &Sandbox{ContainerID: res.ID}
	files := map[string]string{
		"/home/dev/.ws-skills/check/scripts/check.sh": "#!/bin/sh\necho 'it ran'\n",
		"/tmp/empty":     "",
		"/tmp/dir/space": "a\nb\n",
	}
	for p, body := range files {
		if err := m.WriteFile(ctx, sb, p, []byte(body)); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		got, err := m.Exec(ctx, sb, ExecOptions{Cmd: []string{"cat", p}})
		if err != nil || got.ExitCode != 0 || got.Stdout != body {
			t.Errorf("%s: read back %q (exit %d, %v), want %q", p, got.Stdout, got.ExitCode, err, body)
		}
	}
	own, _ := m.Exec(ctx, sb, ExecOptions{Cmd: []string{"stat", "-c", "%U %a", "/home/dev/.ws-skills/check/scripts/check.sh"}})
	if strings.TrimSpace(own.Stdout) != "dev 644" {
		t.Errorf("owner and mode %q, want dev 644", own.Stdout)
	}
}
