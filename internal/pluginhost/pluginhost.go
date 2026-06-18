// Package pluginhost implements the consumer side of clown's plugin
// protocol (clown RFC-0002): plugin directories carry a clown.json
// manifest declaring HTTP MCP servers; each server is launched as a
// subprocess, emits a one-line handshake on stdout naming the address
// it bound, is health-polled until ready, and is then registered with
// the host as a URL-based MCP server. Servers run until their stdin
// closes (the lifetime signal) and are torn down with the host.
package pluginhost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amarbel-llc/trapeze/internal/home"
)

// ManifestName is the plugin manifest file discovered in each plugin
// directory.
const ManifestName = "clown.json"

// claudePluginManifest is the claude-native manifest carrying the
// plugin's identity; used for naming only.
const claudePluginManifest = ".claude-plugin/plugin.json"

// PluginDirsEnv lists additional plugin directories (colon-separated),
// typically baked in by the Nix wrapper (see mkTrapeze in flake.nix).
const PluginDirsEnv = "TRAPEZE_PLUGIN_DIRS"

// Manifest is the parsed clown.json (RFC-0002). Monitors are a
// claude-code concept and are ignored here — the trapeze analog is the
// job-wakeup channel surfaced by internal/jobs.
type Manifest struct {
	Version      int                       `json:"version"`
	HTTPServers  map[string]ServerDef      `json:"httpServers,omitempty"`
	StdioServers map[string]StdioServerDef `json:"stdioServers,omitempty"`
}

// ServerDef declares one HTTP MCP server.
type ServerDef struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Transport   string            `json:"transport,omitempty"` // "streamable-http" (default) or "sse"
	Healthcheck HealthcheckDef    `json:"healthcheck,omitzero"`
	Timeout     int               `json:"timeout,omitempty"` // milliseconds
}

// StdioServerDef declares one stdio MCP server; trapeze speaks stdio
// MCP natively, so these map straight to MCP config entries without a
// subprocess managed here.
type StdioServerDef struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// HealthcheckDef configures readiness polling for an HTTP server.
type HealthcheckDef struct {
	Path     string       `json:"path,omitempty"`    // default "/healthz"
	Interval JSONDuration `json:"interval,omitzero"` // default 1s
	Timeout  JSONDuration `json:"timeout,omitzero"`  // default 30s
}

func (h HealthcheckDef) path() string {
	if h.Path == "" {
		return "/healthz"
	}
	return h.Path
}

func (h HealthcheckDef) interval() time.Duration {
	if h.Interval <= 0 {
		return time.Second
	}
	return time.Duration(h.Interval)
}

func (h HealthcheckDef) timeout() time.Duration {
	if h.Timeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(h.Timeout)
}

// JSONDuration decodes Go duration strings (e.g. "1s", "500ms") from
// JSON.
type JSONDuration time.Duration

func (d *JSONDuration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = JSONDuration(parsed)
	return nil
}

func (d JSONDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// LoadManifest reads and validates a plugin directory's clown.json.
// Returns (nil, nil) when the directory carries no manifest.
func LoadManifest(pluginDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(pluginDir, ManifestName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d", ManifestName, m.Version)
	}
	for name, def := range m.HTTPServers {
		if def.Command == "" {
			return nil, fmt.Errorf("%s: httpServers.%s: command is required", ManifestName, name)
		}
		switch def.Transport {
		case "", "streamable-http", "sse":
		default:
			return nil, fmt.Errorf("%s: httpServers.%s: unsupported transport %q", ManifestName, name, def.Transport)
		}
	}
	for name, def := range m.StdioServers {
		if def.Command == "" {
			return nil, fmt.Errorf("%s: stdioServers.%s: command is required", ManifestName, name)
		}
	}
	return &m, nil
}

// PluginName resolves a plugin directory's identity: the "name" field
// of .claude-plugin/plugin.json when present, else the directory
// basename.
func PluginName(pluginDir string) string {
	data, err := os.ReadFile(filepath.Join(pluginDir, claudePluginManifest))
	if err == nil {
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Name != "" {
			return manifest.Name
		}
	}
	return filepath.Base(pluginDir)
}

// Dirs merges the configured plugin directories with the
// TRAPEZE_PLUGIN_DIRS environment list (colon-separated), dropping
// duplicates and empties.
func Dirs(configured []string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(dir string) {
		dir = home.Long(strings.TrimSpace(dir))
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range configured {
		add(dir)
	}
	for dir := range strings.SplitSeq(os.Getenv(PluginDirsEnv), ":") {
		add(dir)
	}
	return out
}

// EntryName builds the MCP registration name for a plugin server. The
// agent derives LLM tool names from it (mcp_<name>_<tool>), so it is
// sanitized to the [a-zA-Z0-9_-] alphabet providers accept. A server
// named like its plugin collapses to one segment.
func EntryName(plugin, server string) string {
	name := plugin
	if !strings.EqualFold(plugin, server) {
		name = plugin + "_" + server
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '-'
	}, name)
}
