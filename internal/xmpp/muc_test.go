package xmpp

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestIntegrationMUC is an env-gated integration test for the MUC client
// against a real XMPP server with a MUC component (e.g. a dev or krone
// Prosody). It is skipped unless TRAPEZE_XMPP_JID, TRAPEZE_XMPP_PASSWORD, and
// TRAPEZE_XMPP_ROOM are set, so it never runs in the hermetic nix sandbox.
//
// Env vars:
//   - TRAPEZE_XMPP_JID       (required) the account JID, e.g. you@example.net
//   - TRAPEZE_XMPP_PASSWORD  (required) the account password
//   - TRAPEZE_XMPP_ROOM      (required) bare room JID, e.g.
//     test-room@conference.example.net
//   - TRAPEZE_XMPP_SERVER    (optional) host:port dial override
//   - TRAPEZE_XMPP_INSECURE  (optional) "1"/"true" to skip TLS verification
//
// Two clients join the same room under different nicks; one sends a groupchat
// message and the test asserts the other receives it AND that the sender does
// NOT receive its own reflected message (the self-echo suppression the bridge
// relies on).
func TestIntegrationMUC(t *testing.T) {
	jid := os.Getenv("TRAPEZE_XMPP_JID")
	password := os.Getenv("TRAPEZE_XMPP_PASSWORD")
	room := os.Getenv("TRAPEZE_XMPP_ROOM")
	if jid == "" || password == "" || room == "" {
		t.Skip("set TRAPEZE_XMPP_JID, TRAPEZE_XMPP_PASSWORD, and TRAPEZE_XMPP_ROOM to run the MUC integration test")
	}
	server := os.Getenv("TRAPEZE_XMPP_SERVER")
	insecure, _ := strconv.ParseBool(os.Getenv("TRAPEZE_XMPP_INSECURE"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listenerGot := make(chan string, 8)
	listener, err := ConnectMUC(ctx, jid, password, server, insecure, room, "listener", func(nick, body string) {
		t.Logf("listener received from %s: %q", nick, body)
		listenerGot <- body
	})
	if err != nil {
		t.Fatalf("listener join: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	senderEcho := make(chan string, 8)
	sender, err := ConnectMUC(ctx, jid, password, server, insecure, room, "sender", func(_, body string) {
		senderEcho <- body
	})
	if err != nil {
		t.Fatalf("sender join: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	want := "muc integration " + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := sender.SendGroup(ctx, want); err != nil {
		t.Fatalf("send group: %v", err)
	}

	select {
	case got := <-listenerGot:
		deadline := time.After(5 * time.Second)
		for got != want {
			select {
			case got = <-listenerGot:
			case <-deadline:
				t.Fatalf("listener did not receive sent message; last %q want %q", got, want)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for listener to receive the groupchat message")
	}

	// The sender must NOT see its own reflected message (self-echo suppression).
	select {
	case echo := <-senderEcho:
		if echo == want {
			t.Fatalf("sender received its own message %q; self-echo suppression failed", echo)
		}
	case <-time.After(2 * time.Second):
		// Expected: no self-echo delivered.
	}
}
