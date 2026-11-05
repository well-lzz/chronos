// Package missionproc launches whole missions in fail-closed OS sandboxes.
// The supervisor owns authorization; a worker receives only its private root
// and an explicit configuration over stdin, never the supervisor environment.
package missionproc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Process struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

type cappedLog struct {
	mu   sync.Mutex
	file *os.File
	left int
}

func (w *cappedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(p) > w.left {
		p = p[:w.left]
	}
	if len(p) > 0 {
		if _, err := w.file.Write(p); err != nil {
			return 0, err
		}
		w.left -= len(p)
	}
	return n, nil
}

func Start(binary, root string, args []string, bootstrap any) (*Process, error) {
	return StartWithRuntime(binary, root, args, bootstrap, Runtime{})
}

// Runtime grants only explicitly selected, read-only executable assets. It
// must never be populated from model output or an entire home/workspace.
type Runtime struct {
	ReadOnly    []string
	Environment []string
}

func StartWithRuntime(binary, root string, args []string, bootstrap any, runtime Runtime) (*Process, error) {
	binary, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("worker configuration exceeds 1 MiB")
	}
	cmd, err := sandboxCommand(binary, root, args, runtime.ReadOnly)
	if err != nil {
		return nil, err
	}
	cmd.Dir = root
	cmd.Env = []string{"HOME=" + root, "TMPDIR=" + root, "LANG=C", "PATH=/usr/local/bin:/usr/bin:/bin", "SSL_CERT_FILE=/etc/ssl/cert.pem"}
	cmd.Env = append(cmd.Env, runtime.Environment...)
	cmd.Stdin = bytes.NewReader(data)
	log, err := os.OpenFile(filepath.Join(root, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	info, err := log.Stat()
	if err != nil {
		log.Close()
		return nil, err
	}
	left := 2<<20 - int(info.Size())
	if left < 0 {
		left = 0
	}
	writer := &cappedLog{file: log, left: left}
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = log.Close()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *Process) PID() int              { return p.cmd.Process.Pid }
func (p *Process) Done() <-chan struct{} { return p.done }
func (p *Process) Err() error            { <-p.done; p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *Process) Stop(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = syscall.Kill(-p.PID(), syscall.SIGTERM)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-p.PID(), syscall.SIGKILL)
		select {
		case <-p.done:
			return nil
		case <-time.After(3 * time.Second):
			return fmt.Errorf("worker did not exit after SIGKILL")
		}
	}
}

// DecodeBootstrap is deliberately bounded; workers do not read .env or host
// configuration before this explicit private-pipe bootstrap.
func DecodeBootstrap(dst any) error {
	d := json.NewDecoder(io.LimitReader(os.Stdin, (1<<20)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid trailing worker configuration")
	}
	return nil
}
