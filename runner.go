//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const outputLimit = 128 * 1024

type localRunner struct{ goBinary string }

func newLocalRunner(binary string) (*localRunner, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil || !bytes.HasPrefix(out, []byte("go version go")) {
		return nil, fmt.Errorf("GO_BINARY must point to a working Go toolchain")
	}
	return &localRunner{path}, nil
}

type outputBudget struct {
	sync.Mutex
	size     int
	exceeded bool
	cancel   context.CancelFunc
}
type limitedOutput struct {
	budget *outputBudget
	data   bytes.Buffer
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	w.budget.Lock()
	defer w.budget.Unlock()
	n := len(p)
	remaining := outputLimit - w.budget.size
	if n > remaining {
		p = p[:remaining]
		w.budget.exceeded = true
		w.budget.cancel()
	}
	w.data.Write(p)
	w.budget.size += len(p)
	return n, nil
}

// Each command gets its own process group. Kill descendants on every completion,
// including successful parent exits, so ordinary child processes cannot linger.
func runCommand(parent context.Context, limit time.Duration, cmd *exec.Cmd) (string, string, int, bool, bool) {
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	budget := &outputBudget{cancel: cancel}
	stdout, stderr := &limitedOutput{budget: budget}, &limitedOutput{budget: budget}
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", err.Error(), -1, false, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	code := 0
	if err != nil {
		code = -1
		var e *exec.ExitError
		if errors.As(err, &e) {
			code = e.ExitCode()
		}
	}
	return stdout.data.String(), stderr.data.String(), code, ctx.Err() != nil, budget.exceeded
}
func (r *localRunner) Run(ctx context.Context, in request) result {
	dir, err := os.MkdirTemp("", "gomaster-run-")
	if err != nil {
		return result{Status: "internal_error"}
	}
	defer os.RemoveAll(dir)
	source, binary := filepath.Join(dir, "main.go"), filepath.Join(dir, "program")
	if err := os.WriteFile(source, []byte(in.Source), 0600); err != nil {
		return result{Status: "internal_error"}
	}
	cmd := exec.Command(r.goBinary, "build", "-o", binary, source)
	cmd.Dir = dir
	// No server secrets or inherited credentials are passed to compilation or execution.
	cmd.Env = []string{"PATH=" + filepath.Dir(r.goBinary) + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "GOCACHE=" + filepath.Join(os.TempDir(), "gomaster-go-cache"), "GO111MODULE=off", "GOPROXY=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0"}
	out, stderr, code, timed, overflow := runCommand(ctx, 30*time.Second, cmd)
	if code != 0 || timed || overflow {
		status := "compile_error"
		if overflow {
			status = "output_limit_exceeded"
		}
		if ctx.Err() != nil {
			status = "internal_error"
		}
		return result{Status: status, CompileOutput: out + stderr}
	}
	cmd = exec.Command(binary)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewBufferString(in.Stdin)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "GOMAXPROCS=1", "GOMEMLIMIT=128MiB"}
	start := time.Now()
	out, stderr, code, timed, overflow = runCommand(ctx, 3*time.Second, cmd)
	status := "accepted"
	if code != 0 {
		status = "runtime_error"
	}
	if timed {
		status = "time_limit_exceeded"
	}
	if overflow {
		status = "output_limit_exceeded"
	}
	if ctx.Err() != nil {
		status = "internal_error"
	}
	if status == "accepted" && in.Expected != nil && out != *in.Expected {
		status = "wrong_answer"
	}
	return result{Status: status, Stdout: out, Stderr: stderr, CompileOutput: "", ExitCode: &code, TimeMS: time.Since(start).Milliseconds()}
}
