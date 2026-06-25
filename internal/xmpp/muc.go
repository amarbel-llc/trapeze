package xmpp

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/muc"
	"mellium.im/xmpp/mux"
	"mellium.im/xmpp/stanza"
)

// GroupMessageHandler is invoked for each incoming groupchat message in a MUC
// room, with the sender's nick (the resourcepart of the room JID the stanza
// came from) and the message body. Like IncomingHandler it runs on the
// session's read goroutine, so it MUST NOT call back into MUCClient.Send
// (mellium locks the output stream during dispatch); forward to a channel or a
// subprocess instead.
//
// The handler is NOT called for: the joining occupant's own messages (MUC
// reflects them back), room subject stanzas (a <subject> with no <body>),
// bodyless stanzas, error bounces, or delayed history replayed on join.
type GroupMessageHandler func(nick, body string)

// MUCClient is a connected XMPP session joined to a single Multi-User Chat
// room. It is the group-chat analogue of Client: ConnectMUC negotiates the
// session and joins the room, SendGroup posts a groupchat message, and incoming
// messages are delivered to a GroupMessageHandler.
type MUCClient struct {
	session *xmpp.Session
	channel *muc.Channel
	room    jid.JID // bare room JID, e.g. session-x@conference.example.net
	nick    string
}

// groupMessage decodes the parts of an incoming groupchat <message> the bridge
// cares about. encoding/xml fills Body/Subject from the matching child
// elements; Delay is present (Stamp non-empty) when the server is replaying
// room history on join (XEP-0203), which we skip so the bridge does not echo
// the backlog.
type groupMessage struct {
	stanza.Message
	Body    string `xml:"body"`
	Subject string `xml:"subject"`
	Delay   struct {
		Stamp string `xml:"stamp,attr"`
	} `xml:"urn:xmpp:delay delay"`
}

// ConnectMUC negotiates an XMPP session for userJID and joins the MUC room at
// roomJID using nick as the in-room nickname. Incoming groupchat messages are
// forwarded to onGroup. It blocks until the room roster has been received (the
// MUC join handshake), so on return the client is live in the room.
//
// roomJID is the BARE room address (room@service); the nick is appended as the
// resourcepart for the join presence. server/insecureTLS behave as in Connect.
//
// Unlike the 1:1 Connect, this drives the session through a mux so the muc
// package can observe the join/leave presence it needs to track room state;
// groupchat bodies are routed to onGroup via mux.MessageFunc.
func ConnectMUC(ctx context.Context, userJID, password, server string, insecureTLS bool, roomJID, nick string, onGroup GroupMessageHandler) (*MUCClient, error) {
	room, err := jid.Parse(roomJID)
	if err != nil {
		return nil, fmt.Errorf("parse room jid %q: %w", roomJID, err)
	}
	room = room.Bare()
	if nick == "" {
		return nil, fmt.Errorf("muc: nick must not be empty")
	}

	session, addr, err := negotiateSession(ctx, userJID, password, server, insecureTLS)
	if err != nil {
		return nil, err
	}
	slog.Info("XMPP session negotiated for MUC", "jid", addr.String(), "room", room.String())

	mc := &MUCClient{session: session, room: room, nick: nick}

	mucClient := &muc.Client{}
	handler := mux.MessageFunc(
		stanza.GroupChatMessage,
		xml.Name{Local: "body"},
		func(_ stanza.Message, t xmlstream.TokenReadEncoder) error {
			mc.dispatchGroup(t, onGroup)
			return nil
		},
	)
	m := mux.New(stanza.NSClient, muc.HandleClient(mucClient), handler)

	// Announce availability, then serve the mux in the background so the muc
	// client can process the join handshake presence.
	if err := session.Send(ctx, stanza.Presence{Type: stanza.AvailablePresence}.Wrap(nil)); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("send initial presence: %w", err)
	}
	go func() {
		slog.Info("XMPP MUC serve loop starting")
		if err := session.Serve(m); err != nil {
			slog.Error("XMPP MUC session serve ended", "error", err)
		} else {
			slog.Info("XMPP MUC serve loop ended cleanly")
		}
	}()

	// Join blocks until the full room roster has been received.
	occupant, err := room.WithResource(nick)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("muc: build occupant jid: %w", err)
	}
	channel, err := mucClient.Join(ctx, occupant, session, muc.Nick(nick))
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("muc: join %q: %w", room.String(), err)
	}
	mc.channel = channel
	slog.Info("XMPP MUC joined", "room", room.String(), "nick", nick)

	return mc, nil
}

// dispatchGroup decodes a routed groupchat message and forwards it to onGroup
// unless it is self-echo, a subject/bodyless stanza, an error bounce, or
// replayed history. mux has already matched type=groupchat with a <body>, but
// it still hands us subject-and-body stanzas and our own reflected messages.
func (c *MUCClient) dispatchGroup(t xml.TokenReader, onGroup GroupMessageHandler) {
	var msg groupMessage
	if err := xml.NewTokenDecoder(t).Decode(&msg); err != nil && err != io.EOF {
		slog.Error("Failed to decode incoming MUC message", "error", err)
		return
	}
	if msg.Body == "" || msg.Type == stanza.ErrorMessage {
		return
	}
	if msg.Delay.Stamp != "" {
		// Room history replayed on join; do not re-emit the backlog.
		return
	}
	nick := msg.From.Resourcepart()
	if nick == "" || nick == c.nick {
		// A bare room JID (system message) or our own reflected message.
		return
	}
	onGroup(nick, msg.Body)
}

// SendGroup posts a groupchat message with the given body to the room. Safe to
// call from outside the serve handler (e.g. a bridge's inbound goroutine); not
// safe to call from within a GroupMessageHandler.
func (c *MUCClient) SendGroup(ctx context.Context, body string) error {
	payload := xmlstream.Wrap(
		xmlstream.Token(xml.CharData(body)),
		xml.StartElement{Name: xml.Name{Local: "body"}},
	)
	msg := stanza.Message{To: c.room, Type: stanza.GroupChatMessage}
	return c.session.Send(ctx, msg.Wrap(payload))
}

// Room returns the bare JID of the joined room.
func (c *MUCClient) Room() jid.JID { return c.room }

// Close leaves the room (best-effort) and tears down the session.
func (c *MUCClient) Close() error {
	if c.channel != nil {
		ctx, cancel := context.WithCancel(context.Background())
		_ = c.channel.Leave(ctx, "")
		cancel()
	}
	return c.session.Close()
}
