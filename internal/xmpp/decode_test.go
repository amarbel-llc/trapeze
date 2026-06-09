package xmpp

import (
	"encoding/xml"
	"strings"
	"testing"

	"mellium.im/xmlstream"
)

// serveReader replicates how session.Serve presents an incoming stanza to a
// handler: it consumes the outer <message> start element from the raw stream
// and returns that start plus a reader positioned AFTER it (inner children +
// the matching close, via xmlstream.InnerElement). decodeChatMessage must cope
// with exactly this positioning — feeding it the bare inner stream, or using
// DecodeElement(_, start), regresses to "unexpected end element </message>".
func serveReader(t *testing.T, raw string) (*xml.StartElement, xml.TokenReader) {
	t.Helper()
	r := xml.NewDecoder(strings.NewReader(raw))
	for {
		tok, err := r.Token()
		if err != nil {
			t.Fatalf("no start element found in %q: %v", raw, err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return &se, xmlstream.InnerElement(r)
		}
	}
}

func TestDecodeChatMessage(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantFrom string
		wantBody string
		wantOK   bool
	}{
		{
			name:     "chat with body",
			raw:      `<message xmlns="jabber:client" type="chat" from="sasha@example.net/phone" to="me@example.net"><body>hello there</body></message>`,
			wantFrom: "sasha@example.net",
			wantBody: "hello there",
			wantOK:   true,
		},
		{
			name:   "error bounce ignored",
			raw:    `<message xmlns="jabber:client" type="error" from="sasha@example.net"><body>nope</body><error type="cancel"><service-unavailable/></error></message>`,
			wantOK: false,
		},
		{
			name:   "bodyless message ignored",
			raw:    `<message xmlns="jabber:client" type="chat" from="sasha@example.net"><composing xmlns="http://jabber.org/protocol/chatstates"/></message>`,
			wantOK: false,
		},
		{
			name:     "body with child markup",
			raw:      `<message xmlns="jabber:client" type="chat" from="a@b.net/x"><body>hi</body><active xmlns="http://jabber.org/protocol/chatstates"/></message>`,
			wantFrom: "a@b.net",
			wantBody: "hi",
			wantOK:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, inner := serveReader(t, tc.raw)
			from, body, ok := decodeChatMessage(inner, start)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (from=%q body=%q)", ok, tc.wantOK, from, body)
			}
			if !ok {
				return
			}
			if from != tc.wantFrom {
				t.Errorf("from = %q, want %q", from, tc.wantFrom)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}
