package xmpp

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestConnectSendReceive is an env-gated integration test against a real XMPP
// server. It is skipped unless TRAPEZE_XMPP_JID and TRAPEZE_XMPP_PASSWORD are
// set, so it never runs in the hermetic nix sandbox (which has no network).
//
// Env vars:
//   - TRAPEZE_XMPP_JID       (required) the account JID, e.g. you@example.net
//   - TRAPEZE_XMPP_PASSWORD  (required) the account password
//   - TRAPEZE_XMPP_SERVER    (optional) host:port dial override
//   - TRAPEZE_XMPP_CONTACT   (optional) peer to message; defaults to the JID
//     itself (self-message loop), which lets the test also assert receipt
//   - TRAPEZE_XMPP_INSECURE  (optional) "1"/"true" to skip TLS verification
//     (self-signed dev certs)
//
// When the contact is the account's own JID, the test sends a message and
// waits to receive it back, exercising the full connect → send → serve →
// decode → handler path. Otherwise it only asserts connect + send succeed.
func TestConnectSendReceive(t *testing.T) {
	jid := os.Getenv("TRAPEZE_XMPP_JID")
	password := os.Getenv("TRAPEZE_XMPP_PASSWORD")
	if jid == "" || password == "" {
		t.Skip("set TRAPEZE_XMPP_JID and TRAPEZE_XMPP_PASSWORD to run the XMPP integration test")
	}
	server := os.Getenv("TRAPEZE_XMPP_SERVER")
	contact := os.Getenv("TRAPEZE_XMPP_CONTACT")
	selfLoop := contact == ""
	if selfLoop {
		contact = jid
	}
	insecure, _ := strconv.ParseBool(os.Getenv("TRAPEZE_XMPP_INSECURE"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	received := make(chan string, 8)
	client, err := Connect(ctx, jid, password, server, insecure, func(from, body string) {
		t.Logf("received from %s: %q", from, body)
		received <- body
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	want := "trapeze xmpp integration test " + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := client.Send(ctx, contact, want); err != nil {
		t.Fatalf("send: %v", err)
	}

	if !selfLoop {
		t.Logf("sent to %s; not a self-loop, skipping receipt assertion", contact)
		return
	}

	select {
	case got := <-received:
		if got != want {
			// A self-loop may also surface unrelated messages; keep reading
			// briefly for the one we sent.
			deadline := time.After(5 * time.Second)
			for got != want {
				select {
				case got = <-received:
				case <-deadline:
					t.Fatalf("did not receive sent message; last got %q want %q", got, want)
				}
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting to receive the self-sent message")
	}
}
