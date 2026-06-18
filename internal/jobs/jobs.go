package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amarbel-llc/trapeze/internal/pubsub"
)

var (
	latestStates   []*JobState
	latestStatesMu sync.RWMutex
)

// JobState is the UI-facing snapshot of one job on the session's
// channel, derived purely from its journal records (never from
// producer liveness — the RFC-0009 §10 gap applies here too: a
// hard-crashed producer keeps reporting "running").
type JobState struct {
	ID        string
	Source    string
	From      string
	State     string // running | succeeded | failed | cancelled | interrupted | message
	Started   time.Time
	Ended     time.Time
	Progress  string // message of the newest progress record
	Message   string // message of the terminal (or message-type) record
	ResultRef string
}

// Done reports whether the job reached a terminal state.
func (j *JobState) Done() bool {
	return IsTerminal(j.State)
}

// Event is published whenever the channel snapshot changes.
type Event struct {
	States []*JobState
}

var broker = pubsub.NewBroker[Event]()

// SubscribeEvents returns a channel that receives events when the job
// snapshot changes.
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return broker.Subscribe(ctx)
}

// PublishStates publishes a job snapshot event with the given states.
func PublishStates(states []*JobState) {
	broker.Publish(pubsub.UpdatedEvent, Event{States: cloneStates(states)})
}

// cloneStates returns a deep copy of the given state slice so callers
// cannot accidentally mutate the source.
func cloneStates(states []*JobState) []*JobState {
	if states == nil {
		return nil
	}
	result := make([]*JobState, len(states))
	for i, s := range states {
		clone := *s
		result[i] = &clone
	}
	return result
}

// GetLatestStates returns the latest job snapshot.
func GetLatestStates() []*JobState {
	latestStatesMu.RLock()
	defer latestStatesMu.RUnlock()
	return cloneStates(latestStates)
}

// SetLatestStates stores the given states in the package-level cache so
// that GetLatestStates can return them synchronously before the first
// pubsub event arrives.
func SetLatestStates(states []*JobState) {
	latestStatesMu.Lock()
	latestStates = cloneStates(states)
	latestStatesMu.Unlock()
}

// stateFromRecords folds one job's journal records (already seq-sorted)
// into a JobState.
func stateFromRecords(records []Record) *JobState {
	if len(records) == 0 {
		return nil
	}
	s := &JobState{
		ID:    records[0].Job,
		State: StateRunning,
	}
	for _, r := range records {
		switch r.Type {
		case TypeStarted:
			s.Source = r.Source
			s.Started = r.Time()
		case TypeProgress:
			s.Progress = r.Message
		case TypeMessage:
			s.State = TypeMessage
			s.Source = r.Source
			s.From = r.From
			s.Message = r.Message
			s.ResultRef = r.ResultRef
			if s.Started.IsZero() {
				s.Started = r.Time()
			}
		default:
			if IsTerminal(r.Type) {
				s.State = r.Type
				s.Ended = r.Time()
				s.Message = r.Message
				s.ResultRef = r.ResultRef
			}
		}
	}
	return s
}

// ReadChannelStates derives the full snapshot for a channel by reading
// every job journal under its directory. A missing directory is an
// empty channel. The result is sorted newest-started first, running
// jobs before finished ones.
func ReadChannelStates(channelID string) []*JobState {
	dir := JournalDir(channelID)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var states []*JobState
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, ".") {
			continue
		}
		if s := stateFromRecords(readJournal(filepath.Join(dir, name))); s != nil {
			states = append(states, s)
		}
	}
	SortStates(states)
	return states
}

// SortStates orders a snapshot for display: running jobs first, then by
// start time descending, then by id for stability.
func SortStates(states []*JobState) {
	slices.SortStableFunc(states, func(a, b *JobState) int {
		aRunning, bRunning := a.State == StateRunning, b.State == StateRunning
		if aRunning != bRunning {
			if aRunning {
				return -1
			}
			return 1
		}
		if c := b.Started.Compare(a.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// EqualStates reports whether two snapshots are identical. Used by the
// watcher to suppress no-op publishes on rescans.
func EqualStates(a, b []*JobState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if *a[i] != *b[i] {
			return false
		}
	}
	return true
}
