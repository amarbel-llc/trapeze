package xmppbridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	c := Config{Group: "sess1"}
	c.withDefaults()
	if c.Nick != "bridge" {
		t.Errorf("Nick = %q, want bridge", c.Nick)
	}
	if c.ClownBin != "clown" {
		t.Errorf("ClownBin = %q, want clown", c.ClownBin)
	}
	if c.BridgeKey != "xmpp-bridge:sess1" {
		t.Errorf("BridgeKey = %q, want xmpp-bridge:sess1", c.BridgeKey)
	}
	if c.PollInterval != 2*time.Second {
		t.Errorf("PollInterval = %v, want 2s", c.PollInterval)
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hello", "hello"},
		{"first\nsecond", "first"},
		{"", ""},
		{"\nleading", ""},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatOutbound(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    chatMessage
		want string
	}{
		{"subject only", chatMessage{From: "agent", Subject: "done"}, "agent: done"},
		{"subject and body", chatMessage{From: "agent", Subject: "summary", Body: "details here"}, "agent: summary\ndetails here"},
		{"source fallback", chatMessage{Source: "moxy", Subject: "ci green"}, "moxy: ci green"},
		{"body only", chatMessage{From: "agent", Body: "just body"}, "agent: just body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatOutbound(tc.m); got != tc.want {
				t.Errorf("formatOutbound = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsOwnMessage(t *testing.T) {
	b := &Bridge{cfg: Config{BridgeKey: "xmpp-bridge:s1"}}
	if !b.isOwnMessage(chatMessage{From: "xmpp-bridge:s1"}) {
		t.Error("own key not detected")
	}
	if !b.isOwnMessage(chatMessage{Source: bridgeSource}) {
		t.Error("own source not detected")
	}
	if b.isOwnMessage(chatMessage{From: "agent", Source: "claude"}) {
		t.Error("agent message wrongly flagged as own")
	}
}

// fakeClown writes a shell script standing in for the clown binary: `chat send`
// appends its argv to a log; `chat read --json` emits whatever lines the test
// seeded in $FAKE_CLOWN_READ. It lets us exercise the bridge's subprocess
// wiring without a real clown.
func fakeClown(t *testing.T, readJSONL string) (bin, sendLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake clown shell script is POSIX-only")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "clown")
	sendLog = filepath.Join(dir, "send.log")
	readFile := filepath.Join(dir, "read.jsonl")
	if err := os.WriteFile(readFile, []byte(readJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = chat ] && [ \"$2\" = send ]; then\n" +
		"  echo \"$@\" >> " + sendLog + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = chat ] && [ \"$2\" = read ]; then\n" +
		"  cat " + readFile + "\n" +
		"  : > " + readFile + "\n" + // drain: subsequent reads are empty
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return bin, sendLog
}

func TestClownChatSend(t *testing.T) {
	bin, sendLog := fakeClown(t, "")
	b := &Bridge{cfg: Config{ClownBin: bin, Group: "sess1", BridgeKey: "xmpp-bridge:sess1"}}
	if err := b.clownChatSend(context.Background(), "sasha", "fix the build\nplease"); err != nil {
		t.Fatalf("clownChatSend: %v", err)
	}
	logged, err := os.ReadFile(sendLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(logged)
	for _, want := range []string{
		"chat send", "--target sess1", "--from xmpp:sasha",
		"--source " + bridgeSource, "--subject fix the build", "--body fix the build",
	} {
		if !contains(got, want) {
			t.Errorf("send argv %q missing %q", got, want)
		}
	}
}

func TestClownChatRead(t *testing.T) {
	jsonl := `{"from":"agent","source":"claude","scope":"group","subject":"on it"}` + "\n" +
		`not-json` + "\n" +
		`{"from":"agent","source":"claude","scope":"group","subject":"done","body":"all green"}` + "\n"
	bin, _ := fakeClown(t, jsonl)
	b := &Bridge{cfg: Config{ClownBin: bin, Group: "sess1", BridgeKey: "xmpp-bridge:sess1"}}
	msgs, err := b.clownChatRead(context.Background())
	if err != nil {
		t.Fatalf("clownChatRead: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (bad line skipped): %+v", len(msgs), msgs)
	}
	if msgs[0].Subject != "on it" || msgs[1].Body != "all green" {
		t.Errorf("unexpected parse: %+v", msgs)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
