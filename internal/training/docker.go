package training

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// DockerRunner trains in a container on the worker's Docker host: the
// trainer image (infra/training has one built on Unsloth) gets the
// dataset at /data (train.jsonl, eval.jsonl), writes the adapter to
// /out, and reports progress as lines "progress 0.42" on stdout. GPUs
// are requested through the NVIDIA runtime ("all", or a count). The
// worker must run where Docker runs, since /data and /out are bind
// mounts of a directory under Dir.
type DockerRunner struct {
	Client *client.Client
	// Image is the trainer image; a job's config.image overrides it.
	Image string
	// GPUs is "all", a count, or "" for none (CPU, for a smoke test).
	GPUs string
	// Dir holds one directory per job (default data/training).
	Dir string
	// Timeout caps one run (default 6 hours).
	Timeout time.Duration
	// Env is extra environment for the trainer, e.g. HF_TOKEN=$HF_TOKEN
	// expanded by the caller from the process environment: the token
	// never sits in a template or a job row.
	Env []string
}

// NewDockerRunner connects to Docker from the environment.
func NewDockerRunner(ctx context.Context, image, gpus, dir string, env []string) (*DockerRunner, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("training: docker client: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("training: docker ping: %w", err)
	}
	return &DockerRunner{Client: cli, Image: image, GPUs: gpus, Dir: dir, Env: env}, nil
}

var progressRe = regexp.MustCompile(`(?m)^progress\s+([0-9]*\.?[0-9]+)\s*$`)

// Run implements Runner.
func (d *DockerRunner) Run(ctx context.Context, spec RunSpec, progress func(float64, string)) ([]byte, string, error) {
	image := spec.Config.Image
	if image == "" {
		image = d.Image
	}
	if image == "" {
		return nil, "", ErrNoRunner
	}
	dir := d.Dir
	if dir == "" {
		dir = "data/training"
	}
	root, err := filepath.Abs(filepath.Join(dir, spec.JobID.String()))
	if err != nil {
		return nil, "", err
	}
	data, out := filepath.Join(root, "data"), filepath.Join(root, "out")
	for _, p := range []string{data, out} {
		if err := os.MkdirAll(p, 0o777); err != nil {
			return nil, "", err
		}
		_ = os.Chmod(p, 0o777) // the trainer runs as its image's user
	}
	defer os.RemoveAll(root)
	if err := os.WriteFile(filepath.Join(data, "train.jsonl"), spec.Train, 0o644); err != nil {
		return nil, "", err
	}
	if len(spec.Eval) > 0 {
		if err := os.WriteFile(filepath.Join(data, "eval.jsonl"), spec.Eval, 0o644); err != nil {
			return nil, "", err
		}
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := d.ensureImage(ctx, image); err != nil {
		return nil, "", err
	}
	cfg := spec.Config
	env := append([]string{
		"BASE_MODEL=" + spec.BaseModel,
		"ADAPTER_NAME=" + spec.AdapterName,
		"TRAIN_FILE=/data/train.jsonl",
		"EVAL_FILE=/data/eval.jsonl",
		"OUT_DIR=/out",
		"EPOCHS=" + strconv.Itoa(cfg.Epochs),
		"LEARNING_RATE=" + strconv.FormatFloat(cfg.LearningRate, 'g', -1, 64),
		"LORA_RANK=" + strconv.Itoa(cfg.Rank),
		"LORA_ALPHA=" + strconv.Itoa(cfg.Alpha),
		"MAX_SEQ_LEN=" + strconv.Itoa(cfg.MaxSeqLen),
	}, d.Env...)
	hc := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: data, Target: "/data", ReadOnly: true},
			{Type: mount.TypeBind, Source: out, Target: "/out"},
		},
		AutoRemove: false,
	}
	if d.GPUs != "" {
		req := container.DeviceRequest{Capabilities: [][]string{{"gpu"}}, Count: -1}
		if n, err := strconv.Atoi(d.GPUs); err == nil && n > 0 {
			req.Count = n
		}
		hc.Resources.DeviceRequests = []container.DeviceRequest{req}
	}
	res, err := d.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "ws-train-" + strings.ReplaceAll(spec.JobID.String(), "-", "")[:12],
		Config:     &container.Config{Image: image, Env: env, Labels: map[string]string{"ws.training": spec.JobID.String()}},
		HostConfig: hc,
	})
	if err != nil {
		return nil, "", fmt.Errorf("training: create: %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer rcancel()
		_, _ = d.Client.ContainerRemove(rctx, res.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := d.Client.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return nil, "", fmt.Errorf("training: start: %w", err)
	}
	logs, err := d.Client.ContainerLogs(ctx, res.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return nil, "", fmt.Errorf("training: logs: %w", err)
	}
	var buf logBuffer
	buf.onLine = func(line string) {
		if m := progressRe.FindStringSubmatch(line); m != nil && progress != nil {
			if f, err := strconv.ParseFloat(m[1], 64); err == nil {
				progress(f, buf.String())
			}
		}
	}
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = stdcopy.StdCopy(&buf, &buf, logs)
	}()
	wait := d.Client.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var code int64
	select {
	case w := <-wait.Result:
		code = w.StatusCode
	case err := <-wait.Error:
		logs.Close()
		<-copyDone
		return nil, buf.String(), fmt.Errorf("training: wait: %w", err)
	case <-ctx.Done():
		logs.Close()
		<-copyDone
		return nil, buf.String(), ctx.Err()
	}
	logs.Close()
	<-copyDone
	if code != 0 {
		return nil, buf.String(), fmt.Errorf("training: the trainer exited with status %d", code)
	}
	adapter, err := tarDir(out)
	if err != nil {
		return nil, buf.String(), err
	}
	if progress != nil {
		progress(1, buf.String())
	}
	return adapter, buf.String(), nil
}

func (d *DockerRunner) ensureImage(ctx context.Context, image string) error {
	if _, err := d.Client.ImageInspect(ctx, image); err == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	resp, err := d.Client.ImagePull(pctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("training: pull %s: %w", image, err)
	}
	defer resp.Close()
	if err := resp.Wait(pctx); err != nil {
		return fmt.Errorf("training: pull %s: %w", image, err)
	}
	return nil
}

// logBuffer keeps the last 256 KB of trainer output and hands whole
// lines to onLine.
type logBuffer struct {
	mu     sync.Mutex
	b      bytes.Buffer
	line   bytes.Buffer
	onLine func(string)
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.b.Write(p)
	if l.b.Len() > 256<<10 {
		b := l.b.Bytes()
		l.b = *bytes.NewBuffer(append([]byte(nil), b[len(b)-200<<10:]...))
	}
	var lines []string
	for _, c := range p {
		if c == '\n' {
			lines = append(lines, l.line.String())
			l.line.Reset()
			continue
		}
		l.line.WriteByte(c)
	}
	l.mu.Unlock()
	if l.onLine != nil {
		for _, ln := range lines {
			l.onLine(ln)
		}
	}
	return len(p), nil
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tarDir packs a directory (the adapter) as a gzipped tar with paths
// relative to it.
func tarDir(dir string) ([]byte, error) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	n := 0
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("training: the trainer wrote nothing to /out")
	}
	return out.Bytes(), nil
}
