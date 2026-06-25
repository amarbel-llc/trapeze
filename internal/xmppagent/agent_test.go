package xmppagent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/openrouter"
)

type fakeLLM struct {
	mu       sync.Mutex
	lastMsgs []openrouter.Message
	reply    string
	err      error
}

func (f *fakeLLM) Complete(_ context.Context, msgs []openrouter.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMsgs = msgs
	return f.reply, f.err
}

type fakeSender struct {
	mu   sync.Mutex
	sent []struct{ to, body string }
}

func (f *fakeSender) Send(_ context.Context, to, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, struct{ to, body string }{to, body})
	return nil
}

func newTestAgent(l llm, s sender, cfg Config) *Agent {
	return &Agent{cfg: cfg, llm: l, out: s, history: map[string][]openrouter.Message{}}
}

func TestHandleRepliesAndKeepsHistory(t *testing.T) {
	l := &fakeLLM{reply: "answer one"}
	s := &fakeSender{}
	a := newTestAgent(l, s, Config{SystemPrompt: "be terse"})

	a.handle(context.Background(), "sasha@x", "first question")

	if len(s.sent) != 1 || s.sent[0].to != "sasha@x" || s.sent[0].body != "answer one" {
		t.Fatalf("unexpected sent: %+v", s.sent)
	}
	// The model should have seen system + user.
	if l.lastMsgs[0].Role != "system" || l.lastMsgs[0].Content != "be terse" {
		t.Errorf("missing system prompt: %+v", l.lastMsgs)
	}
	if l.lastMsgs[len(l.lastMsgs)-1].Content != "first question" {
		t.Errorf("missing user turn: %+v", l.lastMsgs)
	}

	// Second turn must carry prior user+assistant history.
	l.reply = "answer two"
	a.handle(context.Background(), "sasha@x", "second question")
	roles := []string{}
	for _, m := range l.lastMsgs {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Errorf("history roles = %v, want %v", roles, want)
	}
}

func TestHandlePerPeerIsolation(t *testing.T) {
	l := &fakeLLM{reply: "ok"}
	a := newTestAgent(l, &fakeSender{}, Config{})
	a.handle(context.Background(), "a@x", "hi from a")
	a.handle(context.Background(), "b@x", "hi from b")
	// b's completion must not contain a's message.
	for _, m := range l.lastMsgs {
		if strings.Contains(m.Content, "hi from a") {
			t.Fatalf("peer b saw peer a's history: %+v", l.lastMsgs)
		}
	}
}

func TestHandleErrorNotifiesPeer(t *testing.T) {
	l := &fakeLLM{err: context.DeadlineExceeded}
	s := &fakeSender{}
	a := newTestAgent(l, s, Config{})
	a.handle(context.Background(), "sasha@x", "q")
	if len(s.sent) != 1 || !strings.Contains(s.sent[0].body, "error") {
		t.Fatalf("expected an error notice sent to peer, got %+v", s.sent)
	}
	// A failed turn must NOT record an assistant message in history.
	if len(a.history["sasha@x"]) != 1 { // just the user turn
		t.Errorf("history after error = %d turns, want 1 (user only)", len(a.history["sasha@x"]))
	}
}

func TestTrimBoundsHistory(t *testing.T) {
	a := newTestAgent(&fakeLLM{reply: "r"}, &fakeSender{}, Config{MaxHistory: 4})
	for i := 0; i < 10; i++ {
		a.handle(context.Background(), "p@x", "msg")
	}
	if got := len(a.history["p@x"]); got > 4 {
		t.Errorf("history not bounded: %d > 4", got)
	}
}
