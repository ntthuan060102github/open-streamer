// Package ffmpeg provides a safe FFmpeg subprocess wrapper.
// All processes are started with exec.CommandContext so they are killed
// when the context is cancelled — no orphan processes.
package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Process wraps an FFmpeg subprocess with stdin/stdout pipes.
type Process struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
}

// Start launches FFmpeg with the given arguments.
// Both stdout and stderr are handled: stdout is exposed via Stdout,
// stderr lines are logged (DEBUG level, ERROR for fatal lines).
func Start(ctx context.Context, ffmpegPath string, args []string) (*Process, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg: start: %w", err)
	}

	// drain stderr in a separate goroutine to avoid pipe deadlock
	go drainStderr(stderr)

	return &Process{
		cmd:    cmd,
		Stdin:  stdin,
		Stdout: stdout,
	}, nil
}

// Wait waits for the FFmpeg process to exit and returns any error.
func (p *Process) Wait() error {
	return p.cmd.Wait()
}

// Close terminates the FFmpeg process and closes stdin.
// Safe to call multiple times.
func (p *Process) Close() error {
	if p.Stdin != nil {
		_ = p.Stdin.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	pid := p.cmd.Process.Pid
	if pid <= 0 {
		return nil
	}

	// Send SIGTERM to the whole process group first.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("ffmpeg: term process group: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		// Process exited with non-zero after TERM; treat as closed.
		return nil
	case <-time.After(2 * time.Second):
		// Escalate to SIGKILL for the whole process group.
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("ffmpeg: kill process group: %w", err)
		}
		<-done
		return nil
	}
}

// PID returns the process ID of the running FFmpeg process.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") {
			slog.Error("ffmpeg: stderr", "line", line)
		} else {
			slog.Debug("ffmpeg: stderr", "line", line)
		}
	}
}
