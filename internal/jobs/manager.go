package jobs

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/amarbel-llc/trapeze/internal/pubsub"
)

// rescanInterval bounds how stale the snapshot can get when the nudge
// socket is unavailable (or a datagram was lost — the nudge layer is
// deliberately lossy, RFC-0009 §6). With the socket bound, this is the
// reconcile tick; without it, the only refresh path.
const rescanInterval = 2 * time.Second

// Manager owns the per-process job-channel snapshot: the latest derived
// states, the watcher goroutine that keeps them fresh, and a pubsub
// broker for change events. It mirrors skills.Manager's shape so the
// server/client/TUI plumbing is symmetric.
//
// Package-level helpers (GetLatestStates, SetLatestStates,
// PublishStates, SubscribeEvents) are preserved for callers that share
// a process with the TUI; bridge a Manager to them with
// WithGlobalMirror (single-workspace processes only).
type Manager struct {
	mu     sync.RWMutex
	states []*JobState

	sessionKey string
	channelID  string

	broker       *pubsub.Broker[Event]
	globalMirror bool

	cancel context.CancelFunc
	done   chan struct{}
}

// ManagerOption configures a Manager at construction time.
type ManagerOption func(*Manager)

// WithGlobalMirror causes the manager to forward SetLatestStates and
// PublishStates calls to the package-level cache and broker. Only safe
// when the process hosts at most one Manager (e.g. local mode or the
// client process).
func WithGlobalMirror() ManagerOption {
	return func(m *Manager) {
		m.globalMirror = true
	}
}

// NewManager constructs a Manager seeded with the given snapshot (which
// may be nil). Call Start to begin watching the session's channel; a
// never-started manager (e.g. the client-mode mirror, whose journal
// lives on the server) is just a passive state holder.
func NewManager(states []*JobState, opts ...ManagerOption) *Manager {
	m := &Manager{
		states: cloneStates(states),
		broker: pubsub.NewBroker[Event](),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.globalMirror {
		SetLatestStates(states)
	}
	return m
}

// Disabled reports whether the channel kill switch (RFC-0009's
// CLOWN_DISABLE_JOB_WAKEUP=1) is set.
func Disabled() bool {
	return os.Getenv("CLOWN_DISABLE_JOB_WAKEUP") == "1"
}

// Start resolves the session's channel and launches the watcher
// goroutine. It also exports the resolved CLOWN_SESSION_ID when the
// environment carried no session key, so producers spawned by this
// process (agent shells, MCP servers) target this session's channel —
// the same export clown performs for its plugin servers (RFC-0009 §2).
//
// No-op when the kill switch is set or the manager is already started.
func (m *Manager) Start(ctx context.Context) {
	if Disabled() || m.done != nil {
		return
	}
	m.sessionKey = SessionKey()
	m.channelID = ChannelID(m.sessionKey)
	if os.Getenv("CLOWN_SESSION_ID") == "" {
		if err := os.Setenv("CLOWN_SESSION_ID", m.sessionKey); err != nil {
			slog.Warn("Jobs: failed to export CLOWN_SESSION_ID", "error", err)
		}
	}

	ctx, m.cancel = context.WithCancel(ctx)
	m.done = make(chan struct{})

	// Seed synchronously so the first frame already shows the channel.
	m.PublishStates(ReadChannelStates(m.channelID))

	conn, owns := bindNudge(m.channelID)
	go m.watch(ctx, conn, owns)
}

// SessionKeyValue returns the resolved session key ("" before Start).
func (m *Manager) SessionKeyValue() string { return m.sessionKey }

// ChannelIDValue returns the resolved channel id ("" before Start).
func (m *Manager) ChannelIDValue() string { return m.channelID }

// watch is the push loop: block on the nudge socket (when owned) with a
// rescanInterval deadline, reconciling against the journal on every
// datagram, timeout, or tick. Journal reconciliation is the source of
// truth; the datagram only buys latency.
func (m *Manager) watch(ctx context.Context, conn *net.UnixConn, ownsSocket bool) {
	defer close(m.done)
	defer func() {
		if conn != nil {
			_ = conn.Close()
			if ownsSocket {
				_ = os.Remove(SocketPath(m.channelID))
			}
		}
	}()
	if conn != nil {
		// Unblock the pending ReadFromUnix as soon as the context ends
		// so Shutdown doesn't wait out the read deadline.
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
	}

	var buf [512]byte
	ticker := time.NewTicker(rescanInterval)
	defer ticker.Stop()

	for {
		if conn != nil {
			_ = conn.SetReadDeadline(time.Now().Add(rescanInterval))
			_, _, _ = conn.ReadFromUnix(buf[:])
		} else {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		states := ReadChannelStates(m.channelID)
		m.mu.RLock()
		changed := !EqualStates(states, m.states)
		m.mu.RUnlock()
		if changed {
			m.PublishStates(states)
		}
	}
}

// bindNudge best-effort binds the channel's nudge socket. When the bind
// fails it probes the existing socket with a benign non-waking nudge:
// a live listener (clown's job-watch monitor owns the wake path) means
// trapeze stays in polling mode; a dead one is removed and rebound.
// Returns (nil, false) for polling mode.
func bindNudge(channelID string) (*net.UnixConn, bool) {
	path := SocketPath(channelID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err == nil {
		return conn, true
	}
	if socketAlive(path) {
		slog.Debug("Jobs: nudge socket has a live owner; polling instead", "path", path)
		return nil, false
	}
	_ = os.Remove(path)
	conn, err = net.ListenUnixgram("unixgram", addr)
	if err != nil {
		slog.Debug("Jobs: could not bind nudge socket; polling instead", "path", path, "error", err)
		return nil, false
	}
	return conn, true
}

// socketAlive probes a unixgram socket by sending a journal-only
// ("progress") nudge for a job that does not exist: a live monitor
// rescans its journal and finds nothing — a no-op by design.
func socketAlive(path string) bool {
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return false
	}
	defer c.Close() //nolint:errcheck
	_, err = c.Write([]byte("1|trapeze-probe|progress\n"))
	return err == nil
}

// States returns a clone of the latest snapshot.
func (m *Manager) States() []*JobState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneStates(m.states)
}

// SetLatestStates updates the manager's cached snapshot without
// publishing.
func (m *Manager) SetLatestStates(states []*JobState) {
	m.mu.Lock()
	m.states = cloneStates(states)
	m.mu.Unlock()
	if m.globalMirror {
		SetLatestStates(states)
	}
}

// PublishStates updates the cached snapshot and publishes a change
// event to subscribers (and, with WithGlobalMirror, the package
// globals).
func (m *Manager) PublishStates(states []*JobState) {
	m.mu.Lock()
	m.states = cloneStates(states)
	m.mu.Unlock()
	if m.globalMirror {
		SetLatestStates(states)
	}
	m.broker.Publish(pubsub.UpdatedEvent, Event{States: cloneStates(states)})
	if m.globalMirror {
		PublishStates(states)
	}
}

// SubscribeEvents returns a channel of snapshot-change events.
func (m *Manager) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.broker.Subscribe(ctx)
}

// Shutdown stops the watcher (releasing the nudge socket) and the
// broker.
func (m *Manager) Shutdown() {
	if m.cancel != nil {
		m.cancel()
		select {
		case <-m.done:
		case <-time.After(2 * rescanInterval):
		}
	}
	if m.broker != nil {
		m.broker.Shutdown()
	}
}
