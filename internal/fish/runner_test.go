package fish

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func requireFish(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skip("fish not installed")
	}
}

func TestSplitBackground(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		want   string
		wantBG bool
	}{
		{"sleep 5 &", "sleep 5", true},
		{"sleep 5&", "sleep 5", true},
		{"sleep 5 &  ", "sleep 5", true},
		{"echo hi", "echo hi", false},
		{"true && echo hi", "true && echo hi", false},
		{"true &&", "true &&", false},
		{"echo hi 2>&", "echo hi 2>&", false},
		{"cat <&", "cat <&", false},
		{"&", "&", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, bg := SplitBackground(tt.in)
		require.Equal(t, tt.wantBG, bg, "input %q", tt.in)
		require.Equal(t, tt.want, got, "input %q", tt.in)
	}
}

func TestRunCapturesOutputAndStatus(t *testing.T) {
	t.Parallel()
	requireFish(t)
	r := NewRunner("", t.TempDir())

	res := r.Run(context.Background(), "s1", "echo hello; echo oops >&2")
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, res.Output, "hello")
	require.Contains(t, res.Output, "oops")

	res = r.Run(context.Background(), "s1", "false")
	require.Equal(t, 1, res.ExitCode)
}

func TestRunPersistsCwdPerSession(t *testing.T) {
	t.Parallel()
	requireFish(t)
	home := t.TempDir()
	r := NewRunner("", home)

	sub := t.TempDir()
	res := r.Run(context.Background(), "s1", "cd "+sub)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, sub, res.Cwd)
	require.Equal(t, sub, r.Cwd("s1"))
	// Other sessions keep their own cwd.
	require.Equal(t, home, r.Cwd("s2"))

	// The next command in s1 starts in the new cwd.
	res = r.Run(context.Background(), "s1", "pwd")
	require.Contains(t, res.Output, sub)
}

func TestRunSyntaxErrorKeepsCwd(t *testing.T) {
	t.Parallel()
	requireFish(t)
	home := t.TempDir()
	r := NewRunner("", home)

	res := r.Run(context.Background(), "s1", `echo "unterminated`)
	require.NotEqual(t, 0, res.ExitCode)
	require.Equal(t, home, r.Cwd("s1"))
}

func TestCancelSessionInterruptsForeground(t *testing.T) {
	t.Parallel()
	requireFish(t)
	r := NewRunner("", t.TempDir())

	done := make(chan Result, 1)
	go func() {
		done <- r.Run(context.Background(), "s1", "sleep 30")
	}()

	require.Eventually(t, func() bool {
		return r.IsSessionBusy("s1")
	}, 5*time.Second, 10*time.Millisecond)

	r.CancelSession("s1")
	select {
	case res := <-done:
		require.True(t, res.Canceled)
		require.False(t, r.IsSessionBusy("s1"))
	case <-time.After(10 * time.Second):
		t.Fatal("canceled run did not return")
	}
}

func TestBackgroundJobLifecycle(t *testing.T) {
	t.Parallel()
	requireFish(t)
	r := NewRunner("", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := r.Subscribe(ctx)

	snap, err := r.Start("s1", "echo from-job")
	require.NoError(t, err)
	require.Equal(t, "1", snap.ID)
	require.Equal(t, JobRunning, snap.Status)

	var finished JobSnapshot
	deadline := time.After(10 * time.Second)
	for finished.Status == "" {
		select {
		case ev := <-events:
			if ev.Type == pubsub.UpdatedEvent && ev.Payload.Job.Status.Finished() {
				finished = ev.Payload.Job
			}
		case <-deadline:
			t.Fatal("job did not finish")
		}
	}
	require.Equal(t, JobDone, finished.Status)
	require.Equal(t, 0, finished.ExitCode)
	require.Contains(t, finished.Output, "from-job")

	jobs := r.Jobs()
	require.Len(t, jobs, 1)
	require.Equal(t, JobDone, jobs[0].Status)
}

func TestKillJob(t *testing.T) {
	t.Parallel()
	requireFish(t)
	r := NewRunner("", t.TempDir())

	snap, err := r.Start("s1", "sleep 30")
	require.NoError(t, err)
	require.NoError(t, r.KillJob(snap.ID))

	require.Eventually(t, func() bool {
		jobs := r.Jobs()
		return len(jobs) == 1 && jobs[0].Status == JobKilled
	}, 10*time.Second, 20*time.Millisecond)

	require.Error(t, r.KillJob("999"))
}

func TestCapBuffer(t *testing.T) {
	t.Parallel()
	b := &capBuffer{}
	chunk := strings.Repeat("x", 64*1024)
	for range 8 {
		n, err := b.Write([]byte(chunk))
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
	}
	out := b.String()
	require.LessOrEqual(t, len(out), maxOutputBytes+64)
	require.Contains(t, out, "[output truncated]")
}
