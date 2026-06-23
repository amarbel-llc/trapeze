package jobs

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelID(t *testing.T) {
	t.Parallel()
	// Known vector: lowercase hex of the first 16 bytes of
	// SHA-256("test-session"). Byte-compatibility with clown's
	// internal/jobwake is the whole point — do not change.
	require.Equal(t, "4943e43bc034c8bf90e1c2895796b954", ChannelID("test-session"))
	require.Len(t, ChannelID("anything"), 32)
}

func TestSessionKeyPrecedence(t *testing.T) {
	t.Setenv("CLOWN_SESSION_ID", "from-clown")
	t.Setenv("SPINCLASS_SESSION_ID", "from-spinclass")
	t.Setenv("CLAUDE_SESSION_ID", "from-claude")
	require.Equal(t, "from-clown", SessionKey())

	t.Setenv("CLOWN_SESSION_ID", "")
	require.Equal(t, "from-spinclass", SessionKey())

	t.Setenv("SPINCLASS_SESSION_ID", "")
	require.Equal(t, "from-claude", SessionKey())

	t.Setenv("CLAUDE_SESSION_ID", "")
	generated := SessionKey()
	require.Len(t, generated, 32)
	require.NotEqual(t, generated, SessionKey(), "unset env must generate fresh keys")
}

func writeJournal(t *testing.T, cid, job string, lines ...string) {
	t.Helper()
	dir := JournalDir(cid)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	var data []byte
	for _, l := range lines {
		data = append(data, []byte(l+"\n")...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, job+".jsonl"), data, 0o600))
}

func TestReadChannelStates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const cid = "00112233445566778899aabbccddeeff"

	writeJournal(
		t, cid, "build-1",
		`{"v":1,"job":"build-1","session":"s","source":"moxy","type":"started","seq":0,"ts":"2026-06-10T10:00:00Z"}`,
		`{"v":1,"job":"build-1","session":"s","source":"moxy","type":"progress","seq":1,"ts":"2026-06-10T10:00:05Z","message":"compiling"}`,
	)
	writeJournal(
		t, cid, "merge-2",
		`{"v":1,"job":"merge-2","session":"s","source":"spinclass","type":"started","seq":0,"ts":"2026-06-10T09:00:00Z"}`,
		`{"v":1,"job":"merge-2","session":"s","source":"spinclass","type":"succeeded","seq":1,"ts":"2026-06-10T09:05:00Z","message":"merged","result_ref":"sha256-deadbeef"}`,
	)
	writeJournal(
		t, cid, "note-3",
		`{"v":1,"job":"note-3","session":"s","source":"clown","from":"other-session","type":"message","seq":0,"ts":"2026-06-10T08:00:00Z","message":"hello"}`,
	)
	// Malformed tail line must not hide the job.
	writeJournal(
		t, cid, "broken-4",
		`{"v":1,"job":"broken-4","session":"s","source":"x","type":"started","seq":0,"ts":"2026-06-10T07:00:00Z"}`,
		`{"v":1,"job":"broken-4","ses`,
	)

	states := ReadChannelStates(cid)
	require.Len(t, states, 4)

	// Running jobs sort first, then newest-started first.
	require.Equal(t, "build-1", states[0].ID)
	require.Equal(t, StateRunning, states[0].State)
	require.Equal(t, "moxy", states[0].Source)
	require.Equal(t, "compiling", states[0].Progress)
	require.False(t, states[0].Done())

	require.Equal(t, "broken-4", states[1].ID)
	require.Equal(t, StateRunning, states[1].State)

	require.Equal(t, "merge-2", states[2].ID)
	require.Equal(t, TypeSucceeded, states[2].State)
	require.Equal(t, "merged", states[2].Message)
	require.Equal(t, "sha256-deadbeef", states[2].ResultRef)
	require.True(t, states[2].Done())
	require.Equal(t, 5*time.Minute, states[2].Ended.Sub(states[2].Started))

	require.Equal(t, "note-3", states[3].ID)
	require.Equal(t, TypeMessage, states[3].State)
	require.Equal(t, "other-session", states[3].From)
	require.Equal(t, "hello", states[3].Message)
}

func TestReadChannelStatesMissingDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	require.Empty(t, ReadChannelStates("ffffffffffffffffffffffffffffffff"))
}

func TestManagerNudgeWake(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CLOWN_SESSION_ID", "manager-nudge-wake")
	t.Setenv("CLOWN_DISABLE_JOB_WAKEUP", "")

	cid := ChannelID("manager-nudge-wake")

	m := NewManager(nil)
	m.Start(t.Context())
	defer m.Shutdown()
	require.Equal(t, cid, m.ChannelIDValue())

	events := m.SubscribeEvents(t.Context())

	writeJournal(
		t, cid, "job-a",
		`{"v":1,"job":"job-a","session":"manager-nudge-wake","source":"test","type":"started","seq":0,"ts":"2026-06-10T10:00:00Z"}`,
	)

	// Push path: a nudge datagram must trigger a rescan well before the
	// reconcile ticker would.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: SocketPath(cid), Net: "unixgram"})
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck
	_, err = conn.Write(fmt.Appendf(nil, "%d|job-a|started\n", SchemaVersion))
	require.NoError(t, err)

	select {
	case ev := <-events:
		require.Len(t, ev.Payload.States, 1)
		require.Equal(t, "job-a", ev.Payload.States[0].ID)
		require.Equal(t, StateRunning, ev.Payload.States[0].State)
	case <-time.After(5 * time.Second):
		t.Fatal("no event after nudge")
	}
}

func TestManagerDisabled(t *testing.T) {
	t.Setenv("CLOWN_DISABLE_JOB_WAKEUP", "1")
	m := NewManager(nil)
	m.Start(t.Context())
	defer m.Shutdown()
	require.Empty(t, m.ChannelIDValue())
}

func TestManagerExportsSessionKey(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CLOWN_SESSION_ID", "")
	t.Setenv("SPINCLASS_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")
	t.Setenv("CLOWN_DISABLE_JOB_WAKEUP", "")

	m := NewManager(nil)
	m.Start(t.Context())
	defer m.Shutdown()

	require.NotEmpty(t, m.SessionKeyValue())
	require.Equal(t, m.SessionKeyValue(), os.Getenv("CLOWN_SESSION_ID"),
		"resolved key must be exported so child producers target this channel")
}

func TestBindNudgeRespectsLiveOwner(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const cid = "aaaabbbbccccddddeeeeffff00001111"

	// Simulate clown's job-watch monitor owning the socket.
	owner, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: SocketPath(cid), Net: "unixgram"})
	require.NoError(t, err)
	defer owner.Close() //nolint:errcheck

	conn, owns := bindNudge(cid)
	require.Nil(t, conn, "must fall back to polling when a live monitor owns the socket")
	require.False(t, owns)

	// The probe datagram must be a benign, journal-only nudge.
	require.NoError(t, owner.SetReadDeadline(time.Now().Add(time.Second)))
	var buf [512]byte
	n, _, err := owner.ReadFromUnix(buf[:])
	require.NoError(t, err)
	require.Equal(t, "1|trapeze-probe|progress\n", string(buf[:n]))
}

func TestBindNudgeReclaimsStaleSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const cid = "aaaabbbbccccddddeeeeffff00002222"

	// A crashed monitor leaves the socket file behind with no listener.
	require.NoError(t, os.MkdirAll(filepath.Dir(SocketPath(cid)), 0o700))
	require.NoError(t, os.WriteFile(SocketPath(cid), nil, 0o600))

	conn, owns := bindNudge(cid)
	require.NotNil(t, conn, "stale socket file must be reclaimed")
	require.True(t, owns)
	conn.Close() //nolint:errcheck,gosec
}
