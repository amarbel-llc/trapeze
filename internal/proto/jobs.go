package proto

import "time"

// JobState is the wire representation of jobs.JobState (one job on the
// session's clown job-wakeup channel).
type JobState struct {
	ID        string    `json:"id"`
	Source    string    `json:"source,omitempty"`
	From      string    `json:"from,omitempty"`
	State     string    `json:"state"`
	Started   time.Time `json:"started,omitzero"`
	Ended     time.Time `json:"ended,omitzero"`
	Progress  string    `json:"progress,omitempty"`
	Message   string    `json:"message,omitempty"`
	ResultRef string    `json:"result_ref,omitempty"`
}

// JobsEvent is the wire representation of jobs.Event.
type JobsEvent struct {
	States []JobState `json:"states"`
}
