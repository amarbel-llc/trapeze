package model

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/fish"
	"github.com/charmbracelet/crush/internal/ui/common"
)

// isShellMode reports whether the UI is running as trapeze's shell mode
// (fish-backed commands rendered as tool uses).
func (m *UI) isShellMode() bool {
	return m.com.Config().Shell != nil
}

// shellSessionTitle derives a session title from the session's first
// command: its first line, truncated.
func shellSessionTitle(command string) string {
	const maxLen = 50
	title, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	if len(title) > maxLen {
		title = title[:maxLen] + "…"
	}
	return title
}

// handleShellJobEvent folds a job lifecycle event into the sidebar's
// job list, replacing the entry when the job already exists.
func (m *UI) handleShellJobEvent(ev fish.JobEvent) {
	for i, j := range m.shellJobs {
		if j.ID == ev.Job.ID {
			m.shellJobs[i] = ev.Job
			return
		}
	}
	m.shellJobs = append(m.shellJobs, ev.Job)
}

// jobsInfo renders the background jobs section for the sidebar,
// occupying the spot the skills list has in agent mode.
func (m *UI) jobsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Resource.Heading.Render("Jobs")
	if isSection {
		title = common.Section(t, title, width)
	}

	jobs := m.shellJobs
	if len(jobs) == 0 {
		list := t.Resource.AdditionalText.Render("None")
		return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
	}

	if maxItems > 0 && len(jobs) > maxItems {
		// Most recent jobs are the most interesting; keep the tail.
		jobs = jobs[len(jobs)-maxItems:]
	}

	items := make([]string, 0, len(jobs))
	for _, job := range jobs {
		icon := t.Resource.OnlineIcon.String()
		suffix := ""
		switch job.Status {
		case fish.JobRunning:
			icon = t.Resource.BusyIcon.String()
		case fish.JobFailed:
			icon = t.Resource.ErrorIcon.String()
			suffix = fmt.Sprintf(" (exit %d)", job.ExitCode)
		case fish.JobKilled:
			icon = t.Resource.ErrorIcon.String()
			suffix = " (killed)"
		}
		items = append(items, common.Status(t, common.StatusOpts{
			Icon:  icon,
			Title: t.Resource.Name.Render(fmt.Sprintf("[%s] %s%s", job.ID, job.Command, suffix)),
		}, width))
	}
	list := lipgloss.JoinVertical(lipgloss.Left, items...)
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}
