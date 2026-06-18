package model

import (
	"fmt"
	"slices"

	"charm.land/lipgloss/v2"
	"github.com/amarbel-llc/trapeze/internal/jobs"
	"github.com/amarbel-llc/trapeze/internal/ui/common"
	"github.com/amarbel-llc/trapeze/internal/ui/styles"
)

type jobStatusItem struct {
	icon        string
	name        string
	title       string
	description string
}

// jobsInfo renders the job-wakeup channel section: every job pushed at
// this session over clown's job channel, running first.
func (m *UI) jobsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Resource.Heading.Render("Jobs")
	if isSection {
		title = common.Section(t, title, width)
	}

	items := m.jobStatusItems()
	if len(items) == 0 {
		list := t.Resource.AdditionalText.Render("None")
		return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
	}

	list := jobsList(t, items, width, maxItems)
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

func (m *UI) jobStatusItems() []jobStatusItem {
	t := m.com.Styles
	states := slices.Clone(m.jobStates)
	jobs.SortStates(states)

	items := make([]jobStatusItem, 0, len(states))
	for _, state := range states {
		var icon string
		switch state.State {
		case jobs.StateRunning:
			icon = t.Resource.BusyIcon.String()
		case jobs.TypeSucceeded, jobs.TypeMessage:
			icon = t.Resource.OnlineIcon.String()
		case jobs.TypeFailed:
			icon = t.Resource.ErrorIcon.String()
		case jobs.TypeCancelled:
			icon = t.Resource.DisabledIcon.String()
		default: // interrupted and any future states
			icon = t.Resource.OfflineIcon.String()
		}
		items = append(items, jobStatusItem{
			icon:        icon,
			name:        state.ID,
			title:       t.Resource.Name.Render(state.ID),
			description: jobDescription(state),
		})
	}
	return items
}

// jobDescription picks the one-line status text for a job row: the live
// progress message while running, the terminal/wake message once done,
// falling back to the state word.
func jobDescription(state *jobs.JobState) string {
	switch {
	case state.State == jobs.StateRunning && state.Progress != "":
		return state.Progress
	case state.State == jobs.StateRunning:
		return jobs.StateRunning
	case state.Message != "" && state.From != "":
		return fmt.Sprintf("from %s: %s", state.From, state.Message)
	case state.Message != "":
		return state.Message
	default:
		return state.State
	}
}

func jobsList(t *styles.Styles, items []jobStatusItem, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}

	if len(items) > maxItems {
		visibleItems := items[:maxItems-1]
		remaining := len(items) - (maxItems - 1)
		items = append(visibleItems, jobStatusItem{
			name:  "more",
			title: t.Resource.AdditionalText.Render(fmt.Sprintf("…and %d more", remaining)),
		})
	}

	renderedItems := make([]string, 0, len(items))
	for _, item := range items {
		renderedItems = append(renderedItems, common.Status(t, common.StatusOpts{
			Icon:        item.icon,
			Title:       item.title,
			Description: item.description,
		}, width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, renderedItems...)
}
