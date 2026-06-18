package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/fish"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// Shell mode models the shell interaction after an agent harness: the
// prompt the user submits is a fish command, and instead of running the
// LLM coordinator the workspace synthesizes the same message sequence
// an agent turn would produce — an assistant message carrying a
// finished bash tool call, then a tool message carrying the result —
// so the existing chat UI renders each command as a tool use.
//
// Commands ending in a single trailing '&' become harness-managed
// background jobs: they get a job-start block immediately, show up in
// the sidebar while running (via the runner's JobEvent pubsub,
// forwarded to the TUI), and surface a job-output block in the history
// when they finish.

// SetShellRunner puts the workspace into shell mode with the given
// runner. The provided context bounds the background job watcher.
func (w *AppWorkspace) SetShellRunner(ctx context.Context, r *fish.Runner) {
	w.shell = r
	go w.watchShellJobs(ctx)
}

// ShellCwd returns the shell working directory for a session in shell
// mode, falling back to the workspace working directory.
func (w *AppWorkspace) ShellCwd(sessionID string) string {
	if w.shell == nil || sessionID == "" {
		return w.store.WorkingDir()
	}
	return w.shell.Cwd(sessionID)
}

// shellRun executes one submitted command line. It returns once the
// command block is visible; foreground execution continues on a
// goroutine and lands its result as a tool message.
func (w *AppWorkspace) shellRun(ctx context.Context, sessionID, prompt string) error {
	command := strings.TrimSpace(prompt)
	if command == "" {
		return nil
	}

	cmdStr, background := fish.SplitBackground(command)
	callID := "shell_" + uuid.NewString()
	input, err := json.Marshal(tools.BashParams{
		Command:         cmdStr,
		RunInBackground: background,
	})
	if err != nil {
		return fmt.Errorf("shell: encode command: %w", err)
	}

	// The command renders as a tool use: a single assistant message
	// holding a finished bash tool call. The chat UI shows it as
	// running until the result message lands.
	if _, err := w.app.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID:       callID,
			Name:     tools.BashToolName,
			Input:    string(input),
			Finished: true,
		}},
	}); err != nil {
		return fmt.Errorf("shell: persist command: %w", err)
	}

	if background {
		return w.shellStartJob(ctx, sessionID, callID, cmdStr)
	}

	w.shellMu.Lock()
	w.shellBusy[sessionID]++
	w.shellMu.Unlock()
	go w.shellRunForeground(sessionID, callID, cmdStr)
	return nil
}

// shellRunForeground runs a foreground command to completion and
// persists its result.
func (w *AppWorkspace) shellRunForeground(sessionID, callID, command string) {
	defer func() {
		w.shellMu.Lock()
		w.shellBusy[sessionID]--
		if w.shellBusy[sessionID] <= 0 {
			delete(w.shellBusy, sessionID)
		}
		w.shellMu.Unlock()
	}()

	res := w.shell.Run(context.Background(), sessionID, command)

	exitCode := res.ExitCode
	if res.Canceled && exitCode <= 0 {
		exitCode = 130
	}
	content := res.Output
	if res.Canceled {
		content = strings.TrimRight(content+"\ninterrupted", "\n")
		content = strings.TrimLeft(content, "\n")
	}
	if content == "" {
		content = tools.BashNoOutput
	}
	meta := tools.BashResponseMetadata{
		StartTime:        res.StartedAt.UnixMilli(),
		EndTime:          res.FinishedAt.UnixMilli(),
		Output:           res.Output,
		WorkingDirectory: res.Cwd,
		ExitCode:         exitCode,
	}
	w.shellCreateToolResult(sessionID, callID, tools.BashToolName, content, meta)
}

// shellStartJob launches a background job and immediately lands a
// job-start result on the command's tool call.
func (w *AppWorkspace) shellStartJob(ctx context.Context, sessionID, callID, command string) error {
	snap, err := w.shell.Start(sessionID, command)
	if err != nil {
		meta := tools.BashResponseMetadata{WorkingDirectory: w.shell.Cwd(sessionID)}
		w.shellCreateToolResult(sessionID, callID, tools.BashToolName, fmt.Sprintf("failed to start job: %v", err), meta)
		return nil
	}
	meta := tools.BashResponseMetadata{
		StartTime:        snap.StartedAt.UnixMilli(),
		Description:      command,
		WorkingDirectory: w.shell.Cwd(sessionID),
		Background:       true,
		ShellID:          snap.ID,
	}
	content := fmt.Sprintf("Started background job %s.", snap.ID)
	w.shellCreateToolResult(sessionID, callID, tools.BashToolName, content, meta)
	return nil
}

// watchShellJobs forwards job lifecycle events to the TUI (for the
// sidebar jobs list) and surfaces finished jobs in their session's
// history as a job-output tool use.
func (w *AppWorkspace) watchShellJobs(ctx context.Context) {
	for ev := range w.shell.Subscribe(ctx) {
		w.app.SendEvent(ev)
		if ev.Type != pubsub.UpdatedEvent || !ev.Payload.Job.Status.Finished() {
			continue
		}
		w.shellFinishJob(ctx, ev.Payload.Job)
	}
}

// shellFinishJob appends a job-output block for a finished background
// job to the job's session.
func (w *AppWorkspace) shellFinishJob(ctx context.Context, snap fish.JobSnapshot) {
	callID := "job_" + snap.ID + "_" + uuid.NewString()
	input, err := json.Marshal(tools.JobOutputParams{ShellID: snap.ID})
	if err != nil {
		slog.Error("Shell: encode job output params", "error", err)
		return
	}
	if _, err := w.app.Messages.Create(ctx, snap.SessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID:       callID,
			Name:     tools.JobOutputToolName,
			Input:    string(input),
			Finished: true,
		}},
	}); err != nil {
		slog.Error("Shell: persist job output call", "error", err)
		return
	}

	description := snap.Command
	switch {
	case snap.Status == fish.JobKilled:
		description += " (killed)"
	case snap.ExitCode != 0:
		description += fmt.Sprintf(" (exit %d)", snap.ExitCode)
	}
	content := snap.Output
	if content == "" {
		content = tools.BashNoOutput
	}
	meta := tools.JobOutputResponseMetadata{
		ShellID:     snap.ID,
		Command:     snap.Command,
		Description: description,
		Done:        true,
	}
	w.shellCreateToolResult(snap.SessionID, callID, tools.JobOutputToolName, content, meta)
}

// shellCreateToolResult persists a tool message completing the given
// tool call.
func (w *AppWorkspace) shellCreateToolResult(sessionID, callID, toolName, content string, metadata any) {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		slog.Error("Shell: encode tool result metadata", "error", err)
		metaJSON = []byte("{}")
	}
	if _, err := w.app.Messages.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: callID,
			Name:       toolName,
			Content:    content,
			Metadata:   string(metaJSON),
		}},
	}); err != nil {
		slog.Error("Shell: persist tool result", "error", err)
	}
}

// shellIsBusy reports whether any session has a foreground command
// running.
func (w *AppWorkspace) shellIsBusy() bool {
	w.shellMu.Lock()
	defer w.shellMu.Unlock()
	return len(w.shellBusy) > 0
}

// shellIsSessionBusy reports whether the session has a foreground
// command running.
func (w *AppWorkspace) shellIsSessionBusy(sessionID string) bool {
	w.shellMu.Lock()
	defer w.shellMu.Unlock()
	return w.shellBusy[sessionID] > 0
}
