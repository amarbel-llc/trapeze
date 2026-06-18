// Package fish executes commands through the fish shell for trapeze's
// shell mode. Each submitted command runs in a fresh `fish -c` process;
// the working directory is persisted across commands per session via an
// epilogue that records $PWD, and fish universal variables (set -Ux)
// persist natively through fish's own universal variable store.
//
// Foreground commands run to completion and return their combined
// output; commands ending in a single trailing '&' are harness-managed
// background jobs tracked by the Runner and surfaced over pubsub.
package fish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// DefaultBin is the fish binary used when none is configured.
const DefaultBin = "fish"

// maxOutputBytes caps how much combined output is retained per command
// or job. Output beyond the cap is dropped with a truncation notice.
const maxOutputBytes = 256 * 1024

// cwdEpilogue is appended to every foreground command so the working
// directory survives into the next command. It only runs when the user
// script reaches the end (a syntax error or early exit skips it, in
// which case the previous cwd is kept), and it preserves the user
// command's exit status.
const cwdEpilogue = `
set -l __trapeze_status $status
printf %s $PWD > "$TRAPEZE_CWD_FILE"
exit $__trapeze_status`

// cwdFileEnv is the environment variable carrying the path the
// epilogue writes the final working directory to.
const cwdFileEnv = "TRAPEZE_CWD_FILE"

// Result is the outcome of a foreground command.
type Result struct {
	// Output is the combined stdout+stderr, possibly truncated.
	Output string
	// ExitCode is the command's exit status. -1 when the process was
	// killed before exiting normally.
	ExitCode int
	// Cwd is the working directory after the command ran.
	Cwd string
	// Canceled reports whether the run was interrupted via CancelSession.
	Canceled bool
	// StartedAt and FinishedAt bound the command's execution.
	StartedAt  time.Time
	FinishedAt time.Time
}

// JobStatus is the lifecycle state of a background job.
type JobStatus string

const (
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
	JobKilled  JobStatus = "killed"
)

// Finished reports whether the job has reached a terminal state.
func (s JobStatus) Finished() bool { return s != JobRunning }

// JobSnapshot is an immutable copy of a background job's state, safe to
// hand across goroutines.
type JobSnapshot struct {
	ID         string
	SessionID  string
	Command    string
	Status     JobStatus
	ExitCode   int
	Output     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// JobEvent is published on the Runner's broker whenever a job starts or
// reaches a terminal state.
type JobEvent struct {
	Job JobSnapshot
}

// job is the internal mutable state behind a JobSnapshot.
type job struct {
	mu       sync.Mutex
	snap     JobSnapshot
	out      *capBuffer
	cancel   context.CancelFunc
	doneOnce sync.Once
}

// Runner executes fish commands with per-session working directories
// and tracks harness-managed background jobs.
type Runner struct {
	bin        string
	defaultCwd string

	mu         sync.Mutex
	cwds       map[string]string                  // sessionID -> cwd
	foreground map[string]map[*foregroundRun]bool // sessionID -> running set
	jobs       []*job
	jobsByID   map[string]*job
	nextJobID  int

	broker *pubsub.Broker[JobEvent]
}

type foregroundRun struct {
	cancel context.CancelFunc
}

// NewRunner creates a Runner that executes commands with bin (falling
// back to [DefaultBin]) starting in defaultCwd.
func NewRunner(bin, defaultCwd string) *Runner {
	if bin == "" {
		bin = DefaultBin
	}
	return &Runner{
		bin:        bin,
		defaultCwd: defaultCwd,
		cwds:       make(map[string]string),
		foreground: make(map[string]map[*foregroundRun]bool),
		jobsByID:   make(map[string]*job),
		broker:     pubsub.NewBroker[JobEvent](),
	}
}

// CheckBin verifies the configured fish binary is on PATH.
func (r *Runner) CheckBin() error {
	if _, err := exec.LookPath(r.bin); err != nil {
		return fmt.Errorf("fish shell not found (%q): %w", r.bin, err)
	}
	return nil
}

// Subscribe returns a channel of job lifecycle events.
func (r *Runner) Subscribe(ctx context.Context) <-chan pubsub.Event[JobEvent] {
	return r.broker.Subscribe(ctx)
}

// Cwd returns the current working directory for a session.
func (r *Runner) Cwd(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cwd, ok := r.cwds[sessionID]; ok {
		return cwd
	}
	return r.defaultCwd
}

func (r *Runner) setCwd(sessionID, cwd string) {
	if cwd == "" {
		return
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cwds[sessionID] = cwd
}

// SplitBackground reports whether command requests harness-managed
// backgrounding via a single trailing '&', returning the command with
// the '&' stripped. Trailing '&&', '|&', '>&', and a bare '&' are not
// treated as backgrounding. This is a prototype heuristic: a '&' inside
// a trailing string literal would be misread, but that's rare enough at
// a prompt to accept for now.
func SplitBackground(command string) (string, bool) {
	trimmed := strings.TrimRight(command, " \t")
	if len(trimmed) < 2 || !strings.HasSuffix(trimmed, "&") {
		return command, false
	}
	switch trimmed[len(trimmed)-2] {
	case '&', '|', '>', '<':
		return command, false
	}
	return strings.TrimRight(trimmed[:len(trimmed)-1], " \t"), true
}

// Run executes a foreground command in the session's working directory
// and blocks until it finishes. The run can be interrupted with
// [Runner.CancelSession].
func (r *Runner) Run(ctx context.Context, sessionID, command string) Result {
	start := time.Now()
	cwd := r.Cwd(sessionID)

	cwdFile, err := os.CreateTemp("", "trapeze-cwd-*")
	if err != nil {
		return Result{
			Output:     fmt.Sprintf("trapeze: %v", err),
			ExitCode:   -1,
			Cwd:        cwd,
			StartedAt:  start,
			FinishedAt: time.Now(),
		}
	}
	cwdPath := cwdFile.Name()
	_ = cwdFile.Close()
	defer os.Remove(cwdPath)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := &foregroundRun{cancel: cancel}
	r.trackForeground(sessionID, run)
	defer r.untrackForeground(sessionID, run)

	out := &capBuffer{}
	cmd := r.command(runCtx, cwd, command+cwdEpilogue, out)
	cmd.Env = append(cmd.Environ(), cwdFileEnv+"="+cwdPath)

	exitCode, runErr := runWait(cmd, out)

	canceled := runCtx.Err() != nil
	if newCwd, err := os.ReadFile(cwdPath); err == nil {
		r.setCwd(sessionID, strings.TrimSpace(string(newCwd)))
	}
	output := out.String()
	if output == "" && runErr != nil && !canceled {
		output = runErr.Error()
	}
	return Result{
		Output:     output,
		ExitCode:   exitCode,
		Cwd:        r.Cwd(sessionID),
		Canceled:   canceled,
		StartedAt:  start,
		FinishedAt: time.Now(),
	}
}

// CancelSession interrupts every running foreground command for the
// session. Background jobs are unaffected; use KillJob for those.
func (r *Runner) CancelSession(sessionID string) {
	r.mu.Lock()
	runs := make([]*foregroundRun, 0, len(r.foreground[sessionID]))
	for run := range r.foreground[sessionID] {
		runs = append(runs, run)
	}
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
}

// IsSessionBusy reports whether the session has a foreground command
// running.
func (r *Runner) IsSessionBusy(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.foreground[sessionID]) > 0
}

// IsBusy reports whether any session has a foreground command running.
func (r *Runner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, runs := range r.foreground {
		if len(runs) > 0 {
			return true
		}
	}
	return false
}

func (r *Runner) trackForeground(sessionID string, run *foregroundRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.foreground[sessionID] == nil {
		r.foreground[sessionID] = make(map[*foregroundRun]bool)
	}
	r.foreground[sessionID][run] = true
}

func (r *Runner) untrackForeground(sessionID string, run *foregroundRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.foreground[sessionID], run)
}

// Start launches command as a harness-managed background job in the
// session's current working directory. Jobs do not persist cwd changes
// back to the session.
func (r *Runner) Start(sessionID, command string) (JobSnapshot, error) {
	cwd := r.Cwd(sessionID)
	jobCtx, cancel := context.WithCancel(context.Background())

	out := &capBuffer{}
	cmd := r.command(jobCtx, cwd, command, out)
	if err := cmd.Start(); err != nil {
		cancel()
		return JobSnapshot{}, err
	}

	r.mu.Lock()
	r.nextJobID++
	j := &job{
		snap: JobSnapshot{
			ID:        strconv.Itoa(r.nextJobID),
			SessionID: sessionID,
			Command:   command,
			Status:    JobRunning,
			StartedAt: time.Now(),
		},
		out:    out,
		cancel: cancel,
	}
	r.jobs = append(r.jobs, j)
	r.jobsByID[j.snap.ID] = j
	r.mu.Unlock()

	snap := j.snapshot()
	r.broker.Publish(pubsub.CreatedEvent, JobEvent{Job: snap})

	go func() {
		defer cancel()
		err := cmd.Wait()
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		status := JobDone
		switch {
		case jobCtx.Err() != nil:
			status = JobKilled
		case err != nil || exitCode != 0:
			status = JobFailed
		}
		r.finishJob(j, status, exitCode)
	}()

	return snap, nil
}

func (r *Runner) finishJob(j *job, status JobStatus, exitCode int) {
	j.doneOnce.Do(func() {
		j.mu.Lock()
		j.snap.Status = status
		j.snap.ExitCode = exitCode
		j.snap.FinishedAt = time.Now()
		j.snap.Output = j.out.String()
		snap := j.snap
		j.mu.Unlock()
		r.broker.PublishMustDeliver(context.Background(), pubsub.UpdatedEvent, JobEvent{Job: snap})
	})
}

// KillJob terminates a running background job.
func (r *Runner) KillJob(id string) error {
	r.mu.Lock()
	j, ok := r.jobsByID[id]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such job: %s", id)
	}
	j.cancel()
	return nil
}

// Jobs returns snapshots of all jobs in start order.
func (r *Runner) Jobs() []JobSnapshot {
	r.mu.Lock()
	jobs := make([]*job, len(r.jobs))
	copy(jobs, r.jobs)
	r.mu.Unlock()
	snaps := make([]JobSnapshot, len(jobs))
	for i, j := range jobs {
		snaps[i] = j.snapshot()
	}
	return snaps
}

func (j *job) snapshot() JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	snap := j.snap
	if !snap.Status.Finished() {
		snap.Output = j.out.String()
	}
	return snap
}

// command builds the fish invocation for a script. Stdin is empty (an
// interactive prompt is not a TTY for children yet; PTY support is a
// later pass).
func (r *Runner) command(ctx context.Context, cwd, script string, out *capBuffer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.bin, "-c", script)
	cmd.Dir = cwd
	cmd.Stdout = out
	cmd.Stderr = out
	// Bound the post-exit wait for I/O so a grandchild holding the
	// output pipe can't hang the run forever.
	cmd.WaitDelay = 2 * time.Second
	setupProcessGroup(cmd)
	return cmd
}

// runWait starts and waits for cmd, returning its exit code.
func runWait(cmd *exec.Cmd, out *capBuffer) (int, error) {
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err := cmd.Wait()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode(), err
	}
	return -1, err
}

// capBuffer is a size-capped, concurrency-safe output buffer. Writes
// past the cap are dropped and a single truncation notice is appended.
type capBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.truncated {
		return n, nil
	}
	remaining := maxOutputBytes - b.buf.Len()
	if remaining <= 0 || len(p) > remaining {
		if remaining > 0 {
			b.buf.Write(p[:remaining])
		}
		b.truncated = true
		b.buf.WriteString("\n[output truncated]")
		return n, nil
	}
	b.buf.Write(p)
	return n, nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
