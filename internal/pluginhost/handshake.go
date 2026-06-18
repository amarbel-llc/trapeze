package pluginhost

import (
	"fmt"
	"strconv"
	"strings"
)

// Handshake is the parsed form of the one-line announcement a plugin
// HTTP server prints on stdout once it is listening (RFC-0002 §2):
//
//	<core-version>|<app-version>|<network-type>|<network-address>|<protocol>\n
//
// e.g. "1|1|tcp|127.0.0.1:9000|streamable-http". Fields beyond the
// fifth are ignored for forward compatibility.
type Handshake struct {
	CoreVersion int
	AppVersion  int
	NetworkType string // "tcp"
	Address     string // host:port
	Protocol    string // "streamable-http" or "sse"
}

// ParseHandshake parses a handshake line. The core version must be 1
// and the network type tcp.
func ParseHandshake(line string) (Handshake, error) {
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) < 5 {
		return Handshake{}, fmt.Errorf("handshake: expected 5 fields, got %d in %q", len(fields), line)
	}
	core, err := strconv.Atoi(fields[0])
	if err != nil {
		return Handshake{}, fmt.Errorf("handshake: core version: %w", err)
	}
	if core != 1 {
		return Handshake{}, fmt.Errorf("handshake: unsupported core version %d", core)
	}
	app, err := strconv.Atoi(fields[1])
	if err != nil {
		return Handshake{}, fmt.Errorf("handshake: app version: %w", err)
	}
	h := Handshake{
		CoreVersion: core,
		AppVersion:  app,
		NetworkType: fields[2],
		Address:     fields[3],
		Protocol:    fields[4],
	}
	if h.NetworkType != "tcp" {
		return Handshake{}, fmt.Errorf("handshake: unsupported network type %q", h.NetworkType)
	}
	if h.Address == "" {
		return Handshake{}, fmt.Errorf("handshake: empty address in %q", line)
	}
	switch h.Protocol {
	case "streamable-http", "sse":
	default:
		return Handshake{}, fmt.Errorf("handshake: unsupported protocol %q", h.Protocol)
	}
	return h, nil
}

// URL renders the MCP endpoint URL: /mcp for streamable-http, /sse for
// sse.
func (h Handshake) URL() string {
	path := "/mcp"
	if h.Protocol == "sse" {
		path = "/sse"
	}
	return "http://" + h.Address + path
}
