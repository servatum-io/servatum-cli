// Package runner executes the wrapped backup command, passes its output
// through untouched and keeps the tail for the report.
package runner

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Result struct {
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Output     string // last TailBytes of combined stdout+stderr
	Err        error  // command could not start
}

const TailBytes = 8 * 1024

// Run starts argv, mirrors stdout/stderr, forwards SIGINT/SIGTERM/SIGHUP to
// the child and returns once it exits. Exit code 127 = could not start.
func Run(argv []string, stdout, stderr io.Writer) Result {
	res := Result{StartedAt: time.Now().UTC()}
	buf := &ring{max: TailBytes}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(stdout, buf)
	cmd.Stderr = io.MultiWriter(stderr, buf)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		res.FinishedAt = time.Now().UTC()
		res.ExitCode = 127
		res.Err = err
		res.Output = err.Error()
		return res
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	res.FinishedAt = time.Now().UTC()
	res.Output = buf.String()
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode < 0 { // killed by signal
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.ExitCode = 128 + int(ws.Signal())
			} else {
				res.ExitCode = 1
			}
		}
	default:
		res.ExitCode = 1
		res.Err = err
	}
	return res
}

// ring keeps only the last max bytes written to it.
type ring struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	if over := len(r.b) - r.max; over > 0 {
		r.b = append(r.b[:0], r.b[over:]...)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.b)
}
