package pluginhost

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMain doubles as the fake plugin server: when re-exec'd with
// PLUGINHOST_TEST_SERVER=1 the test binary binds a loopback HTTP
// server, prints the clown handshake on stdout, and blocks until stdin
// closes — the exact lifecycle the protocol prescribes.
func TestMain(m *testing.M) {
	if os.Getenv("PLUGINHOST_TEST_SERVER") == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake server:", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() { _ = http.Serve(ln, mux) }()
	fmt.Printf("1|1|tcp|%s|streamable-http\n", ln.Addr())
	// Block until the host closes stdin (the lifetime signal).
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return
		}
	}
}

func TestParseHandshake(t *testing.T) {
	t.Parallel()
	h, err := ParseHandshake("1|1|tcp|127.0.0.1:9000|streamable-http\n")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9000", h.Address)
	require.Equal(t, "http://127.0.0.1:9000/mcp", h.URL())

	h, err = ParseHandshake("1|1|tcp|127.0.0.1:9001|sse|extra|fields")
	require.NoError(t, err, "fields beyond the fifth are ignored")
	require.Equal(t, "http://127.0.0.1:9001/sse", h.URL())

	for _, bad := range []string{
		"",
		"1|1|tcp|127.0.0.1:9000",            // too few fields
		"2|1|tcp|127.0.0.1:9000|sse",        // wrong core version
		"1|1|udp|127.0.0.1:9000|sse",        // wrong network type
		"1|1|tcp||sse",                      // empty address
		"1|1|tcp|127.0.0.1:9000|grpc",       // unknown protocol
		"x|1|tcp|127.0.0.1:9000|sse",        // non-integer version
		"ready: listening on 127.0.0.1:900", // not a handshake at all
	} {
		_, err := ParseHandshake(bad)
		require.Error(t, err, "line %q must not parse", bad)
	}
}

func TestEntryName(t *testing.T) {
	t.Parallel()
	require.Equal(t, "moxy_grit", EntryName("moxy", "grit"))
	require.Equal(t, "moxy", EntryName("moxy", "moxy"), "plugin-named server collapses")
	require.Equal(t, "my-plugin_a-b_c", EntryName("my.plugin", "a:b_c"))
}

func TestDirs(t *testing.T) {
	t.Setenv(PluginDirsEnv, "/env/one:/env/two::/cfg/one")
	dirs := Dirs([]string{"/cfg/one", "", "/cfg/two"})
	require.Equal(t, []string{"/cfg/one", "/cfg/two", "/env/one", "/env/two"}, dirs)
}

func TestLoadManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	m, err := LoadManifest(dir)
	require.NoError(t, err)
	require.Nil(t, m, "no clown.json means no plugin")

	write := func(content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), []byte(content), 0o600))
	}

	write(`{"version":1,"httpServers":{"srv":{"command":"/bin/server","transport":"sse","healthcheck":{"path":"/ping","interval":"250ms","timeout":"5s"},"timeout":86400000}},"stdioServers":{"cli":{"command":"/bin/cli","args":["serve"]}}}`)
	m, err = LoadManifest(dir)
	require.NoError(t, err)
	srv := m.HTTPServers["srv"]
	require.Equal(t, "sse", srv.Transport)
	require.Equal(t, "/ping", srv.Healthcheck.path())
	require.Equal(t, 250*time.Millisecond, srv.Healthcheck.interval())
	require.Equal(t, 5*time.Second, srv.Healthcheck.timeout())
	require.Equal(t, 86400000, srv.Timeout)
	require.Equal(t, []string{"serve"}, m.StdioServers["cli"].Args)

	write(`{"version":2,"httpServers":{}}`)
	_, err = LoadManifest(dir)
	require.ErrorContains(t, err, "unsupported version")

	write(`{"version":1,"httpServers":{"srv":{}}}`)
	_, err = LoadManifest(dir)
	require.ErrorContains(t, err, "command is required")

	write(`{"version":1,"httpServers":{"srv":{"command":"/bin/x","transport":"grpc"}}}`)
	_, err = LoadManifest(dir)
	require.ErrorContains(t, err, "unsupported transport")
}

func writePluginDir(t *testing.T, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), []byte(manifest), 0o600))
	return dir
}

func TestHostLaunchAndShutdown(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	dir := writePluginDir(t, "fakeplugin", fmt.Sprintf(
		`{"version":1,"httpServers":{"jobs":{"command":%q,"env":{"PLUGINHOST_TEST_SERVER":"1"},"healthcheck":{"interval":"50ms","timeout":"10s"}}},"stdioServers":{"cli":{"command":"/bin/cli"}}}`,
		exe))

	host := New()
	entries, err := host.Launch(t.Context(), []string{dir, t.TempDir() /* manifest-less dir is skipped */})
	require.NoError(t, err)
	require.Len(t, entries, 2)

	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	stdio := byName["fakeplugin_cli"]
	require.Equal(t, "stdio", stdio.Transport)
	require.Equal(t, "/bin/cli", stdio.Command)

	httpEntry := byName["fakeplugin_jobs"]
	require.Equal(t, "streamable-http", httpEntry.Transport)
	require.True(t, strings.HasPrefix(httpEntry.URL, "http://127.0.0.1:"), httpEntry.URL)
	require.True(t, strings.HasSuffix(httpEntry.URL, "/mcp"), httpEntry.URL)

	// The fake server only answers /healthz once actually listening, so
	// reaching here proves handshake + health. Shutdown must reap the
	// process promptly via the stdin-close lifetime signal.
	start := time.Now()
	host.Shutdown()
	require.Less(t, time.Since(start), shutdownGrace, "stdin close should end the server before the SIGTERM grace")
}

func TestHostLaunchFailsOnImmediateExit(t *testing.T) {
	dir := writePluginDir(t, "brokenplugin",
		`{"version":1,"httpServers":{"srv":{"command":"/bin/false"}}}`)

	host := New()
	defer host.Shutdown()
	_, err := host.Launch(t.Context(), []string{dir})
	require.ErrorContains(t, err, "exited before handshake")
}
