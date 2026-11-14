package driver

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Process is the shared lifecycle logic for boxes attached to a host process (runsc run, tsbox __init in
// the dev driver).
type Process struct {
	Cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File

	done    chan struct{}
	mu      sync.Mutex
	err     error
	cleanup func()
	once    sync.Once
}

// StartProcess starts cmd with pipes for stdin/stdout. cleanup runs exactly once after the end.
// stdout is a separate pipe (not cmd.StdoutPipe) so that Wait does not close it before the reader has
// read the last frames (exit).
func StartProcess(cmd *exec.Cmd, stderr io.Writer, cleanup func()) (*Process, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	p := &Process{Cmd: cmd, stdin: stdin, stdout: r, done: make(chan struct{}), cleanup: cleanup}
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		p.runCleanup()
		return nil, fmt.Errorf("starting box: %w", err)
	}
	return p, nil
}

// Wait must be called by the driver in a goroutine. exitErr can translate the error (e.g. the dev
// driver's RAM watchdog).
func (p *Process) Wait(exitErr func(error) error) {
	err := p.Cmd.Wait()
	if exitErr != nil {
		err = exitErr(err)
	}
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	p.runCleanup()
	close(p.done)
}

func (p *Process) runCleanup() {
	p.once.Do(func() {
		if p.cleanup != nil {
			p.cleanup()
		}
	})
}

func (p *Process) Stdin() io.WriteCloser { return p.stdin }
func (p *Process) Stdout() io.Reader     { return p.stdout }
func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Release closes the read end of stdout (after Done).
func (p *Process) Release() { _ = p.stdout.Close() }

// PID is the process ID of the box's host process (runsc run or init).
func (p *Process) PID() int { return p.Cmd.Process.Pid }
