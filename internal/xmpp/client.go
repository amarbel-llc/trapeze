// Package xmpp is a minimal XMPP chat client wrapping mellium.im/xmpp.
//
// It is the backend for trapeze's XMPP mode: Connect dials a client-to-server
// session, Send emits a 1:1 chat message stanza, and incoming chat messages
// are delivered to an IncomingHandler callback. This is the vertical-slice
// surface — 1:1 chat only, no roster/presence-subscription/MUC yet.
package xmpp

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

// IncomingHandler is invoked for each incoming chat message, with the sender's
// bare JID and the message body. It runs on the session's read goroutine, so
// it MUST NOT call back into Client.Send (mellium locks the output stream
// during dispatch — sending from the handler deadlocks). Forwarding to a DB
// write / channel is safe.
type IncomingHandler func(from, body string)

// messageBody is a chat message stanza carrying a <body>, used to DECODE
// incoming messages. Outgoing messages are built with stanza.Message.Wrap (see
// Send) rather than this struct: encoding/xml marshaling of an embedded
// stanza.Message emits a namespaceless <message> with an empty from="" attr
// (jid.JID is a struct, so the omitempty tag does not fire), which servers
// drop when routing to a remote contact. The Wrap path emits the correct
// xmlns="jabber:client", an auto-generated id, and omits an empty from.
type messageBody struct {
	stanza.Message
	Body string `xml:"body"`
}

// decodeChatMessage extracts the sender bare JID and body text from an
// incoming <message> stanza, given the stream reader and start element exactly
// as session.Serve presents them: `start` is the already-consumed <message>
// start element, and `t` is positioned AFTER it (yielding the inner children
// plus the matching close — see xmlstream.InnerElement). It returns ok=false
// for non-chat messages, error-type bounces, and bodyless messages.
//
// The reader must be reconstructed into a full element (start prepended to the
// inner stream) before decoding the whole struct; decoding the bare inner
// stream, or DecodeElement(_, start) against it, fails with "unexpected end
// element </message>". This is the regression guarded by client_test.go.
func decodeChatMessage(t xml.TokenReader, start *xml.StartElement) (from, body string, ok bool) {
	full := xmlstream.MultiReader(xmlstream.Token(xml.StartElement(*start)), t)
	var decoded messageBody
	if err := xml.NewTokenDecoder(full).Decode(&decoded); err != nil && err != io.EOF {
		slog.Error("Failed to decode incoming XMPP message", "error", err)
		return "", "", false
	}
	if decoded.Body == "" || decoded.Type == stanza.ErrorMessage {
		return "", "", false
	}
	return decoded.From.Bare().String(), decoded.Body, true
}

// Client is a connected XMPP client-to-server session.
type Client struct {
	session *xmpp.Session
}

// Connect dials and negotiates an XMPP client session for userJID, sends
// initial available presence, and starts serving incoming stanzas in a
// background goroutine. Incoming chat messages are forwarded to onIncoming.
//
// server is an optional "host:port" override for the TCP dial; when empty,
// mellium resolves the server from the JID's domain (SRV records). insecureTLS
// skips certificate verification — needed for local servers with self-signed
// certs (e.g. a dev Prosody), and a security footgun anywhere else.
func Connect(ctx context.Context, userJID, password, server string, insecureTLS bool, onIncoming IncomingHandler) (*Client, error) {
	addr, err := jid.Parse(userJID)
	if err != nil {
		return nil, fmt.Errorf("parse jid %q: %w", userJID, err)
	}

	tlsCfg := &tls.Config{
		ServerName:         addr.Domain().String(),
		InsecureSkipVerify: insecureTLS, //nolint:gosec // opt-in for dev servers; see insecureTLS doc.
	}

	// Listed in XMPP negotiation order: encrypt (StartTLS), then authenticate
	// (SASL; PLAIN last as a fallback, SCRAM preferred), then bind a resource.
	// mellium negotiates against what the server advertises, but the
	// conventional order makes the intent explicit.
	features := []xmpp.StreamFeature{
		xmpp.StartTLS(tlsCfg),
		xmpp.SASL("", password, sasl.ScramSha256, sasl.ScramSha1, sasl.Plain),
		xmpp.BindResource(),
	}

	var session *xmpp.Session
	if server != "" {
		conn, derr := new(net.Dialer).DialContext(ctx, "tcp", server)
		if derr != nil {
			return nil, fmt.Errorf("dial %q: %w", server, derr)
		}
		session, err = xmpp.NewClientSession(ctx, addr, conn, features...)
	} else {
		session, err = xmpp.DialClientSession(ctx, addr, features...)
	}
	if err != nil {
		return nil, fmt.Errorf("negotiate xmpp session: %w", err)
	}

	c := &Client{session: session}

	slog.Info("XMPP session negotiated", "jid", addr.String())

	// Announce availability so the server routes messages to us.
	if err := session.Send(ctx, stanza.Presence{Type: stanza.AvailablePresence}.Wrap(nil)); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("send initial presence: %w", err)
	}
	slog.Info("XMPP initial presence sent")

	// Forward incoming chat messages (a <message> with a non-empty <body>) to
	// onIncoming, keyed by the sender's bare JID. A raw HandlerFunc (rather
	// than a typed mux) keeps the handler simple and sees every stanza,
	// including type="error" bounces, which it ignores along with non-message
	// stanzas (presence/IQ) for the 1:1-chat slice.
	handler := xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
		slog.Info("XMPP inbound stanza", "name", start.Name.Local)
		if start.Name.Local != "message" {
			return nil
		}
		from, body, ok := decodeChatMessage(t, start)
		if !ok {
			return nil
		}
		onIncoming(from, body)
		return nil
	})

	// Serve blocks pumping the input stream; run it in the background. When it
	// returns (connection closed / error) the session is done.
	go func() {
		slog.Info("XMPP serve loop starting")
		if err := session.Serve(handler); err != nil {
			slog.Error("XMPP session serve ended", "error", err)
		} else {
			slog.Info("XMPP serve loop ended cleanly")
		}
	}()

	return c, nil
}

// Send emits a 1:1 chat message with the given body to the bare JID `to`. Safe
// for concurrent use and safe to call from outside the Serve handler (e.g. the
// TUI goroutine).
func (c *Client) Send(ctx context.Context, to, body string) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("parse recipient jid %q: %w", to, err)
	}
	// Build the stanza via Message.Wrap so it serializes correctly (proper
	// jabber:client namespace, auto-generated id, no empty from=""). The
	// payload is a <body> element wrapping the message text. Send is
	// fire-and-forget (unlike SendMessageElement, which blocks for a reply).
	payload := xmlstream.Wrap(
		xmlstream.Token(xml.CharData(body)),
		xml.StartElement{Name: xml.Name{Local: "body"}},
	)
	msg := stanza.Message{To: toJID, Type: stanza.ChatMessage}
	return c.session.Send(ctx, msg.Wrap(payload))
}

// Close tears down the XMPP session.
func (c *Client) Close() error {
	return c.session.Close()
}

// ValidJID reports whether s parses as an XMPP JID. Used to distinguish a
// peer-JID session title from a generic title like "New Session".
func ValidJID(s string) bool {
	if s == "" {
		return false
	}
	_, err := jid.Parse(s)
	return err == nil
}
