// Package xmppbridge bridges one XMPP Multi-User Chat (MUC) room to one clown
// cross-session chat channel, so a human in an XMPP client can converse with an
// agent running under a spinclass/clown session.
//
// This is the vertical-slice bridge (one room <-> one session, see the trapeze
// XMPP-MUC prototype). Two directions:
//
//   - inbound  (human -> agent): each groupchat message becomes a
//     `clown chat send --target <group>` so the agent's clown job-watch monitor
//     wakes it with the line.
//   - outbound (agent -> human): the bridge polls `clown chat read --json` and
//     posts new messages (those NOT originating from the bridge) into the room.
//
// The bridge does not link clown as a library; it shells out to the `clown`
// binary, keeping clown the sole owner of the chat channel and its on-disk
// format. The bridge runs with its own clown session identity
// (CLOWN_SESSION_ID = a stable bridge key) but the agent's group
// (CLOWN_GROUP_ID = the spinclass session id), so its own messages are
// self-suppressed by clown's group read while the agent's surface to it.
package xmppbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/xmpp"
)

// Config configures a single MUC <-> clown-chat bridge.
type Config struct {
	// XMPP connection.
	JID      string // the bridge bot's bare JID, e.g. bridge@example.net
	Password string
	Server   string // optional host:port dial override
	Insecure bool   // skip TLS verification (dev servers only)
	Room     string // bare room JID, e.g. session-x@conference.example.net
	Nick     string // in-room nickname (default "bridge")

	// clown wiring.
	ClownBin  string // path/name of the clown binary (default "clown")
	Group     string // CLOWN_GROUP_ID / chat --target: the spinclass session id
	BridgeKey string // CLOWN_SESSION_ID for the bridge's own clown identity

	// PollInterval is how often the bridge drains `clown chat read`
	// (default 2s). Outbound latency is bounded by this.
	PollInterval time.Duration
}

func (c *Config) withDefaults() {
	if c.Nick == "" {
		c.Nick = "bridge"
	}
	if c.ClownBin == "" {
		c.ClownBin = "clown"
	}
	if c.BridgeKey == "" {
		c.BridgeKey = "xmpp-bridge:" + c.Group
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
}

// chatMessage mirrors the JSON emitted by `clown chat read --json`
// (jobwake.ChatMessage). Only the fields the bridge uses are kept.
type chatMessage struct {
	From    string `json:"from"`
	Source  string `json:"source"`
	Scope   string `json:"scope"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Bridge is a running MUC <-> clown-chat bridge.
type Bridge struct {
	cfg Config
	muc *xmpp.MUCClient
}

// Run connects to XMPP, joins the room, and runs both bridge directions until
// ctx is cancelled. It blocks for the lifetime of the bridge.
func Run(ctx context.Context, cfg Config) error {
	cfg.withDefaults()
	if cfg.Group == "" {
		return fmt.Errorf("xmppbridge: Group (clown chat target) is required")
	}

	b := &Bridge{cfg: cfg}

	// Inbound: a groupchat message -> clown chat send. The handler runs on the
	// XMPP read goroutine; clownChatSend only execs a subprocess (no XMPP
	// send), so it is safe to call here.
	onGroup := func(nick, body string) {
		if err := b.clownChatSend(ctx, nick, body); err != nil {
			slog.Error("bridge: clown chat send failed", "error", err, "nick", nick)
		}
	}

	muc, err := xmpp.ConnectMUC(ctx, cfg.JID, cfg.Password, cfg.Server, cfg.Insecure, cfg.Room, cfg.Nick, onGroup)
	if err != nil {
		return fmt.Errorf("xmppbridge: connect: %w", err)
	}
	b.muc = muc
	defer muc.Close()

	slog.Info("bridge up", "room", cfg.Room, "group", cfg.Group, "bridgeKey", cfg.BridgeKey)

	// Drain any chat backlog without echoing it into the room: advance the
	// clown read cursor once at startup so only messages produced after the
	// bridge comes up are posted.
	if _, err := b.clownChatRead(ctx); err != nil {
		slog.Warn("bridge: initial chat drain failed", "error", err)
	}

	return b.pollOutbound(ctx)
}

// pollOutbound drains `clown chat read` on a ticker and posts agent messages to
// the room until ctx is done.
func (b *Bridge) pollOutbound(ctx context.Context) error {
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			msgs, err := b.clownChatRead(ctx)
			if err != nil {
				slog.Error("bridge: clown chat read failed", "error", err)
				continue
			}
			for _, m := range msgs {
				if b.isOwnMessage(m) {
					continue
				}
				if err := b.muc.SendGroup(ctx, formatOutbound(m)); err != nil {
					slog.Error("bridge: post to room failed", "error", err)
				}
			}
		}
	}
}

// isOwnMessage reports whether a chat message originated from the bridge itself
// (its own inbound relays), to avoid echoing human messages back into the room.
func (b *Bridge) isOwnMessage(m chatMessage) bool {
	return m.From == b.cfg.BridgeKey || m.Source == bridgeSource
}

const bridgeSource = "xmpp-bridge"

// formatOutbound renders a clown chat message for the room: "sender: subject"
// with the body appended on its own line when present.
func formatOutbound(m chatMessage) string {
	sender := m.From
	if sender == "" {
		sender = m.Source
	}
	line := m.Subject
	if m.Body != "" {
		if line != "" {
			line += "\n"
		}
		line += m.Body
	}
	if sender == "" {
		return line
	}
	return sender + ": " + line
}

// clownChatSend relays an inbound room message to the agent via
// `clown chat send`. The first line becomes the subject (the wake), and the
// full text the body.
func (b *Bridge) clownChatSend(ctx context.Context, nick, body string) error {
	subject := firstLine(body)
	args := []string{
		"chat", "send",
		"--target", b.cfg.Group,
		"--from", "xmpp:" + nick,
		"--source", bridgeSource,
		"--subject", subject,
	}
	if body != subject {
		args = append(args, "--body", body)
	}
	cmd := b.clownCmd(ctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clownChatRead drains the bridge's clown chat inbox (own/group/broadcast),
// advancing the read cursor.
func (b *Bridge) clownChatRead(ctx context.Context) ([]chatMessage, error) {
	cmd := b.clownCmd(ctx, "chat", "read", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var msgs []chatMessage
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m chatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Warn("bridge: skipping unparseable chat line", "error", err)
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, sc.Err()
}

// clownCmd builds a clown subprocess with the bridge's clown identity injected:
// CLOWN_SESSION_ID is the bridge's own key (so its relays are self-suppressed on
// read) and CLOWN_GROUP_ID is the agent's group (so target/group resolution
// lines up with the session the agent runs under).
func (b *Bridge) clownCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, b.cfg.ClownBin, args...)
	cmd.Env = append(cmd.Environ(),
		"CLOWN_SESSION_ID="+b.cfg.BridgeKey,
		"CLOWN_GROUP_ID="+b.cfg.Group,
	)
	return cmd
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
