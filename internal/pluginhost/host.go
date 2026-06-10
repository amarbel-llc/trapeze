package pluginhost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// handshakeTimeout bounds how long a freshly launched server may take
// to print its handshake line.
const handshakeTimeout = 10 * time.Second

// shutdownGrace is how long a server gets between SIGTERM and SIGKILL.
const shutdownGrace = 3 * time.Second

// Entry is one MCP server registration produced by launching plugins:
// either a launched HTTP server (URL set) or a pass-through stdio
// declaration (Command set).
type Entry struct {
	Name      string // sanitized registration name (see EntryName)
	Plugin    string
	Server    string
	Transport string // "streamable-http", "sse", or "stdio"
	URL       string
	Command   string
	Args      []string
	Env       map[string]string
	TimeoutMS int // from the manifest's timeout field (milliseconds)
}

// Host owns the launched plugin server processes.
type Host struct {
	mu    sync.Mutex
	procs []*proc
}

type proc struct {
	plugin string
	server string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
}

// New returns an empty Host.
func New() *Host {
	return &Host{}
}

// Launch discovers clown.json manifests in the given plugin
// directories, starts every declared HTTP server, waits for handshakes
// and health, and returns the MCP entries to register. Servers from all
// plugins launch concurrently. On error, everything already launched is
// shut down.
func (h *Host) Launch(ctx context.Context, pluginDirs []string) ([]Entry, error) {
	type result struct {
		entry Entry
		err   error
	}

	var entries []Entry
	var launches []func() result

	for _, dir := range pluginDirs {
		manifest, err := LoadManifest(dir)
		if err != nil {
			h.Shutdown()
			return nil, fmt.Errorf("plugin %s: %w", dir, err)
		}
		if manifest == nil {
			continue
		}
		plugin := PluginName(dir)

		for _, server := range slices.Sorted(maps.Keys(manifest.StdioServers)) {
			def := manifest.StdioServers[server]
			entries = append(entries, Entry{
				Name:      EntryName(plugin, server),
				Plugin:    plugin,
				Server:    server,
				Transport: "stdio",
				Command:   def.Command,
				Args:      def.Args,
				Env:       def.Env,
			})
		}

		for _, server := range slices.Sorted(maps.Keys(manifest.HTTPServers)) {
			def := manifest.HTTPServers[server]
			launches = append(launches, func() result {
				entry, err := h.launchHTTP(ctx, plugin, server, def)
				return result{entry, err}
			})
		}
	}

	results := make([]result, len(launches))
	var wg sync.WaitGroup
	for i, launch := range launches {
		wg.Go(func() { results[i] = launch() })
	}
	wg.Wait()

	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		entries = append(entries, r.entry)
	}
	if len(errs) > 0 {
		h.Shutdown()
		return nil, errors.Join(errs...)
	}
	return entries, nil
}

// launchHTTP starts one HTTP server subprocess, reads its handshake,
// and polls its health endpoint until ready.
func (h *Host) launchHTTP(ctx context.Context, plugin, server string, def ServerDef) (Entry, error) {
	label := plugin + "/" + server

	cmd := exec.Command(def.Command, def.Args...)
	cmd.Env = os.Environ()
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Own process group (where supported) so shutdown can signal the
	// server and any children it spawned in one go.
	setProcessGroup(cmd)

	// The open stdin pipe is the server's lifetime signal: it must run
	// until stdin closes (RFC-0002 §3).
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", label, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", label, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", label, err)
	}
	if err := cmd.Start(); err != nil {
		return Entry{}, fmt.Errorf("%s: start: %w", label, err)
	}

	h.mu.Lock()
	h.procs = append(h.procs, &proc{plugin: plugin, server: server, cmd: cmd, stdin: stdin})
	h.mu.Unlock()

	// Forward server stderr into the log, line by line.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Info("Plugin server stderr", "plugin", plugin, "server", server, "line", scanner.Text())
		}
	}()

	// First stdout line is the handshake; everything after is drained.
	lines := make(chan string, 1)
	reader := bufio.NewReader(stdout)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			close(lines)
			return
		}
		lines <- line
		_, _ = io.Copy(io.Discard, reader)
	}()

	var line string
	select {
	case l, ok := <-lines:
		if !ok {
			return Entry{}, fmt.Errorf("%s: exited before handshake", label)
		}
		line = l
	case <-time.After(handshakeTimeout):
		return Entry{}, fmt.Errorf("%s: no handshake within %s", label, handshakeTimeout)
	case <-ctx.Done():
		return Entry{}, ctx.Err()
	}

	hs, err := ParseHandshake(line)
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", label, err)
	}
	transport := def.Transport
	if transport == "" {
		transport = "streamable-http"
	}
	if hs.Protocol != transport {
		return Entry{}, fmt.Errorf("%s: handshake protocol %q does not match declared transport %q", label, hs.Protocol, transport)
	}

	if err := waitHealthy(ctx, hs.Address, def.Healthcheck); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", label, err)
	}

	slog.Info("Plugin server ready", "plugin", plugin, "server", server, "url", hs.URL())
	return Entry{
		Name:      EntryName(plugin, server),
		Plugin:    plugin,
		Server:    server,
		Transport: transport,
		URL:       hs.URL(),
		TimeoutMS: def.Timeout,
	}, nil
}

// waitHealthy polls the server's health endpoint until it returns 200,
// the healthcheck timeout elapses, or ctx ends.
func waitHealthy(ctx context.Context, address string, hc HealthcheckDef) error {
	url := "http://" + address + hc.path()
	client := &http.Client{Timeout: hc.interval()}
	deadline := time.Now().Add(hc.timeout())
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy at %s within %s", url, hc.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hc.interval()):
		}
	}
}

// Shutdown stops every launched server: close stdin (the protocol's
// lifetime signal), SIGTERM the process group, and SIGKILL after a
// grace period.
func (h *Host) Shutdown() {
	h.mu.Lock()
	procs := h.procs
	h.procs = nil
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(func() {
			_ = p.stdin.Close()
			terminate(p.cmd)

			done := make(chan struct{})
			go func() {
				_ = p.cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(shutdownGrace):
				kill(p.cmd)
				<-done
			}
			slog.Debug("Plugin server stopped", "plugin", p.plugin, "server", p.server)
		})
	}
	wg.Wait()
}
