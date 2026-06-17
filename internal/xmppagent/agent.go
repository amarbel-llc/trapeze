// Package xmppagent is trapeze's headless, pure-conversational XMPP agent: it
// joins XMPP as a 1:1 chat client, and for each incoming direct message it runs
// an OpenRouter chat completion (carrying per-peer history) and replies.
//
// This is the trapeze half of the "headless clown provider" prototype: clown's
// trapeze provider boots `trapeze xmpp-agent` with the OpenRouter backend and
// XMPP frontend, no TUI. Pure chat — no tools, no file access.
package xmppagent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/charmbracelet/crush/internal/openrouter"
	"github.com/charmbracelet/crush/internal/xmpp"
)

// llm is the completion surface the agent needs (satisfied by
// *openrouter.Client); an interface so tests can substitute a fake.
type llm interface {
	Complete(ctx context.Context, messages []openrouter.Message) (string, error)
}

// sender is the outbound XMPP surface (satisfied by *xmpp.Client).
type sender interface {
	Send(ctx context.Context, to, body string) error
}

// Config configures a headless XMPP agent.
type Config struct {
	// XMPP connection.
	JID      string
	Password string
	Server   string
	Insecure bool

	// OpenRouter backend.
	BaseURL string
	APIKey  string
	Model   string

	// SystemPrompt seeds every conversation (optional).
	SystemPrompt string

	// MaxHistory bounds the per-peer turns kept (user+assistant messages,
	// excluding the system prompt). 0 means a default of 40.
	MaxHistory int
}

// Agent is a running headless XMPP conversational agent.
type Agent struct {
	cfg Config
	llm llm
	out sender

	mu      sync.Mutex
	history map[string][]openrouter.Message // keyed by peer bare JID

	// incoming carries (from, body) off the XMPP serve goroutine so the LLM
	// call + reply Send happen on a worker, never inside the serve handler
	// (mellium deadlocks if you Send from within the handler).
	incoming chan inbound
}

type inbound struct {
	from string
	body string
}

func (c *Config) maxHistory() int {
	if c.MaxHistory <= 0 {
		return 40
	}
	return c.MaxHistory
}

// Run connects to XMPP and serves the conversational loop until ctx is
// cancelled. It blocks for the agent's lifetime.
func Run(ctx context.Context, cfg Config) error {
	if cfg.APIKey == "" {
		return fmt.Errorf("xmppagent: OpenRouter API key is required")
	}
	if cfg.Model == "" {
		return fmt.Errorf("xmppagent: OpenRouter model is required")
	}

	a := &Agent{
		cfg:      cfg,
		llm:      openrouter.New(cfg.BaseURL, cfg.APIKey, cfg.Model),
		history:  map[string][]openrouter.Message{},
		incoming: make(chan inbound, 64),
	}

	client, err := xmpp.Connect(ctx, cfg.JID, cfg.Password, cfg.Server, cfg.Insecure, func(from, body string) {
		// Runs on the XMPP serve goroutine: only enqueue, never block on the LLM.
		select {
		case a.incoming <- inbound{from: from, body: body}:
		case <-ctx.Done():
		}
	})
	if err != nil {
		return fmt.Errorf("xmppagent: connect: %w", err)
	}
	defer client.Close()
	a.out = client

	slog.Info("xmpp agent up", "jid", cfg.JID, "model", cfg.Model)
	return a.serve(ctx)
}

// serve drains the incoming queue, completing and replying to each message.
func (a *Agent) serve(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in := <-a.incoming:
			a.handle(ctx, in.from, in.body)
		}
	}
}

// handle appends the user turn, completes against the model with per-peer
// history, replies, and records the assistant turn. Errors are reported back to
// the peer so a chat client always gets a response.
func (a *Agent) handle(ctx context.Context, from, body string) {
	messages := a.appendUser(from, body)

	reply, err := a.llm.Complete(ctx, messages)
	if err != nil {
		slog.Error("xmppagent: completion failed", "error", err, "peer", from)
		if sendErr := a.out.Send(ctx, from, "⚠️ error: "+err.Error()); sendErr != nil {
			slog.Error("xmppagent: failed to send error notice", "error", sendErr, "peer", from)
		}
		return
	}

	if err := a.out.Send(ctx, from, reply); err != nil {
		slog.Error("xmppagent: failed to send reply", "error", err, "peer", from)
		return
	}
	a.appendAssistant(from, reply)
}

// appendUser records the user turn and returns the full message slice to send
// (system prompt + bounded history).
func (a *Agent) appendUser(from, body string) []openrouter.Message {
	a.mu.Lock()
	defer a.mu.Unlock()

	turns := append(a.history[from], openrouter.Message{Role: "user", Content: body})
	turns = a.trim(turns)
	a.history[from] = turns

	out := make([]openrouter.Message, 0, len(turns)+1)
	if a.cfg.SystemPrompt != "" {
		out = append(out, openrouter.Message{Role: "system", Content: a.cfg.SystemPrompt})
	}
	out = append(out, turns...)
	return out
}

func (a *Agent) appendAssistant(from, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history[from] = a.trim(append(a.history[from], openrouter.Message{Role: "assistant", Content: content}))
}

// trim caps the per-peer history to the most recent MaxHistory turns.
func (a *Agent) trim(turns []openrouter.Message) []openrouter.Message {
	max := a.cfg.maxHistory()
	if len(turns) <= max {
		return turns
	}
	return append([]openrouter.Message(nil), turns[len(turns)-max:]...)
}
