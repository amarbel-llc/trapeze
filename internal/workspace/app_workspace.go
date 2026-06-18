package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent"
	mcptools "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fish"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/xmpp"
)

// AppWorkspace implements the Workspace interface by delegating
// directly to an in-process [app.App] instance. This is the default
// mode when the client/server architecture is not enabled.
type AppWorkspace struct {
	app   *app.App
	store *config.ConfigStore

	// xmpp, when non-nil, puts the workspace in XMPP chat mode: AgentRun
	// sends a chat stanza (and persists the outgoing message) instead of
	// running the LLM coordinator, and AgentIsReady reports the XMPP session
	// as ready. See SetXMPP.
	xmpp *xmpp.Client

	// xmppSessions maps a peer's bare JID to the session.Session that holds
	// that 1:1 conversation. Each peer is its own session (Title = bare JID)
	// so the TUI session-switcher presents them as distinct DM threads.
	// Guarded by xmppMu because inbound (XMPP serve goroutine) and outbound
	// (TUI goroutine) both resolve sessions through it.
	xmppMu       sync.Mutex
	xmppSessions map[string]string

	// shell, when non-nil, puts the workspace in shell mode: AgentRun
	// executes the prompt as a fish command and synthesizes tool-use
	// messages instead of running the LLM coordinator. See SetShellRunner
	// and shell.go.
	shell *fish.Runner

	// shellBusy counts running foreground commands per session, guarded
	// by shellMu. It backs AgentIsBusy/AgentIsSessionBusy in shell mode.
	shellMu   sync.Mutex
	shellBusy map[string]int
}

// NewAppWorkspace creates a new AppWorkspace wrapping the given app
// and config store.
func NewAppWorkspace(a *app.App, store *config.ConfigStore) *AppWorkspace {
	return &AppWorkspace{
		app:          a,
		store:        store,
		xmppSessions: make(map[string]string),
		shellBusy:    make(map[string]int),
	}
}

// SetXMPP puts the workspace into XMPP chat mode with the given connected
// client. It pre-seeds the JID→session cache from existing sessions (whose
// Title is the peer JID) so conversations survive restarts.
func (w *AppWorkspace) SetXMPP(client *xmpp.Client) {
	sessions, err := w.app.Sessions.List(context.Background())
	if err != nil {
		slog.Warn("XMPP: failed to list sessions for JID cache seed", "error", err)
	}
	w.xmppMu.Lock()
	defer w.xmppMu.Unlock()
	w.xmpp = client
	for _, s := range sessions {
		// Only sessions whose title is a peer JID are XMPP conversations;
		// skip leftover non-JID titles (e.g. old "New Session" rows) so they
		// can't be mistaken for a peer's session.
		if xmpp.ValidJID(s.Title) {
			w.xmppSessions[s.Title] = s.ID
		}
	}
}

// resolveXMPPSession returns the session bound to peerJID, creating one
// (Title = peerJID) the first time a peer is seen. Safe for concurrent use.
func (w *AppWorkspace) resolveXMPPSession(ctx context.Context, peerJID string) (string, error) {
	w.xmppMu.Lock()
	defer w.xmppMu.Unlock()
	if id, ok := w.xmppSessions[peerJID]; ok {
		return id, nil
	}
	sess, err := w.app.Sessions.Create(ctx, peerJID)
	if err != nil {
		return "", err
	}
	w.xmppSessions[peerJID] = sess.ID
	return sess.ID, nil
}

// HandleIncomingXMPP routes an incoming chat message into the session for its
// sender (creating that session on first contact) and persists it as the peer
// side of the conversation. Called from the XMPP serve goroutine.
func (w *AppWorkspace) HandleIncomingXMPP(from, body string) {
	// Ignore messages with no/empty or malformed sender JID (server
	// announcements, carbons without a from, etc.) — routing them by JID would
	// create an orphan Title="" session that never surfaces in the TUI.
	if !xmpp.ValidJID(from) {
		slog.Warn("XMPP: dropping incoming message with invalid sender", "from", from)
		return
	}
	ctx := context.Background()
	sessionID, err := w.resolveXMPPSession(ctx, from)
	if err != nil {
		slog.Error("XMPP: failed to resolve session for incoming message", "from", from, "error", err)
		return
	}
	if _, err := w.app.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: body}},
	}); err != nil {
		slog.Error("XMPP: failed to persist incoming message", "from", from, "error", err)
	}
}

// XMPPSessionID returns a session to open the TUI on initially: the most
// recently updated peer (JID-titled) session, or "" if there are none yet
// (landing screen). Non-JID sessions (e.g. leftover coder sessions) are
// skipped so XMPP mode never opens on an unrelated thread.
func (w *AppWorkspace) XMPPSessionID() string {
	sessions, err := w.app.Sessions.List(context.Background())
	if err != nil {
		return ""
	}
	// Sessions.List is ordered most-recent-first (see session service); the
	// first JID-titled one is the most-recent peer conversation.
	for _, s := range sessions {
		if xmpp.ValidJID(s.Title) {
			return s.ID
		}
	}
	return ""
}

// -- Sessions --

func (w *AppWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	return w.app.Sessions.Create(ctx, title)
}

func (w *AppWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	return w.app.Sessions.Get(ctx, sessionID)
}

func (w *AppWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	return w.app.Sessions.List(ctx)
}

func (w *AppWorkspace) SaveSession(ctx context.Context, sess session.Session) (session.Session, error) {
	return w.app.Sessions.Save(ctx, sess)
}

func (w *AppWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	return w.app.Sessions.Delete(ctx, sessionID)
}

func (w *AppWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return w.app.Sessions.CreateAgentToolSessionID(messageID, toolCallID)
}

func (w *AppWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	return w.app.Sessions.ParseAgentToolSessionID(sessionID)
}

// SetCurrentSession is a no-op in single-client local mode. The
// presence concept only matters when multiple clients can share a
// workspace via the HTTP server.
func (w *AppWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	return nil
}

// -- Messages --

func (w *AppWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	// Drain any debounced updates so the caller observes the latest
	// in-memory state. message.Service buffers streaming deltas and a
	// cold List would otherwise miss them at session-switch time.
	if err := w.app.Messages.FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages.List(ctx, sessionID)
}

func (w *AppWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return w.app.Messages.ListUserMessages(ctx, sessionID)
}

func (w *AppWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.app.Messages.ListAllUserMessages(ctx)
}

// -- Agent --

func (w *AppWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	// Shell mode: execute the prompt as a fish command. See shell.go.
	if w.shell != nil {
		return w.shellRun(ctx, sessionID, prompt)
	}

	// XMPP mode: send to the active conversation's peer and persist our side.
	// The peer is the session Title when it is a JID (i.e. a real conversation,
	// inbound-created or already retitled). For a generic session (a fresh
	// "New Session" the TUI made on the landing screen) fall back to the
	// configured default contact and retitle the session to that JID so it
	// becomes a proper per-peer thread. The LLM coordinator is bypassed.
	if w.xmpp != nil {
		sess, err := w.app.Sessions.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("xmpp: look up session: %w", err)
		}
		peerJID := sess.Title
		needsRetitle := false
		if !xmpp.ValidJID(peerJID) {
			cfg := w.store.Config()
			if cfg.XMPP == nil || cfg.XMPP.Contact == "" {
				return errors.New("xmpp: this conversation has no peer JID and no default xmpp.contact is configured")
			}
			peerJID = cfg.XMPP.Contact
			needsRetitle = true
		}
		// Cache the JID→session mapping BEFORE the (best-effort) retitle, so a
		// concurrent inbound reply from this peer on the serve goroutine
		// resolves to this same session instead of racing the Rename and
		// creating a duplicate.
		w.xmppMu.Lock()
		w.xmppSessions[peerJID] = sessionID
		w.xmppMu.Unlock()
		if needsRetitle {
			// Retitle the generic session to the peer JID so future restarts
			// recover the mapping and the switcher shows the contact.
			if err := w.app.Sessions.Rename(ctx, sessionID, peerJID); err != nil {
				slog.Warn("XMPP: failed to retitle session to peer JID", "error", err)
			}
		}
		if _, err := w.app.Messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: prompt}},
		}); err != nil {
			return fmt.Errorf("persist outgoing message: %w", err)
		}
		if err := w.xmpp.Send(ctx, peerJID, prompt); err != nil {
			slog.Error("XMPP send failed", "to", peerJID, "error", err)
			return err
		}
		return nil
	}

	if w.app.AgentCoordinator == nil {
		return errors.New("agent coordinator not initialized")
	}
	_, err := w.app.AgentCoordinator.Run(ctx, sessionID, prompt, attachments...)
	return err
}

func (w *AppWorkspace) AgentCancel(sessionID string) {
	if w.shell != nil {
		w.shell.CancelSession(sessionID)
		return
	}
	if w.app.AgentCoordinator != nil {
		w.app.AgentCoordinator.Cancel(sessionID)
	}
}

func (w *AppWorkspace) AgentIsBusy() bool {
	if w.shell != nil {
		return w.shellIsBusy()
	}
	if w.app.AgentCoordinator == nil {
		return false
	}
	return w.app.AgentCoordinator.IsBusy()
}

func (w *AppWorkspace) AgentIsSessionBusy(sessionID string) bool {
	if w.shell != nil {
		return w.shellIsSessionBusy(sessionID)
	}
	if w.app.AgentCoordinator == nil {
		return false
	}
	return w.app.AgentCoordinator.IsSessionBusy(sessionID)
}

func (w *AppWorkspace) AgentModel() AgentModel {
	if w.app.AgentCoordinator == nil {
		return AgentModel{}
	}
	m := w.app.AgentCoordinator.Model()
	return AgentModel{
		CatwalkCfg: m.CatwalkCfg,
		ModelCfg:   m.ModelCfg,
	}
}

func (w *AppWorkspace) AgentIsReady() bool {
	// In XMPP mode the connected session is what "ready" means; there is no
	// coordinator. This gates ui.sendMessage, so it must be true to send.
	if w.xmpp != nil {
		return true
	}
	// Same for shell mode: a runner means commands can be executed.
	if w.shell != nil {
		return true
	}
	return w.app.AgentCoordinator != nil
}

func (w *AppWorkspace) AgentQueuedPrompts(sessionID string) int {
	if w.app.AgentCoordinator == nil {
		return 0
	}
	return w.app.AgentCoordinator.QueuedPrompts(sessionID)
}

func (w *AppWorkspace) AgentQueuedPromptsList(sessionID string) []string {
	if w.app.AgentCoordinator == nil {
		return nil
	}
	return w.app.AgentCoordinator.QueuedPromptsList(sessionID)
}

func (w *AppWorkspace) AgentClearQueue(sessionID string) {
	if w.app.AgentCoordinator != nil {
		w.app.AgentCoordinator.ClearQueue(sessionID)
	}
}

func (w *AppWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	if w.app.AgentCoordinator == nil {
		return errors.New("agent coordinator not initialized")
	}
	return w.app.AgentCoordinator.Summarize(ctx, sessionID)
}

func (w *AppWorkspace) UpdateAgentModel(ctx context.Context) error {
	return w.app.UpdateAgentModel(ctx)
}

func (w *AppWorkspace) InitCoderAgent(ctx context.Context) error {
	// No LLM agent in XMPP or shell mode.
	if w.xmpp != nil || w.shell != nil {
		return nil
	}
	return w.app.InitCoderAgent(ctx)
}

func (w *AppWorkspace) GetDefaultSmallModel(providerID string) config.SelectedModel {
	return w.app.GetDefaultSmallModel(providerID)
}

// -- Permissions --

func (w *AppWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return w.app.Permissions.Grant(perm)
}

func (w *AppWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return w.app.Permissions.GrantPersistent(perm)
}

func (w *AppWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return w.app.Permissions.Deny(perm)
}

func (w *AppWorkspace) PermissionSkipRequests() bool {
	return w.app.Permissions.SkipRequests()
}

func (w *AppWorkspace) PermissionSetSkipRequests(skip bool) {
	w.app.Permissions.SetSkipRequests(skip)
}

// -- FileTracker --

func (w *AppWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	w.app.FileTracker.RecordRead(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.app.FileTracker.ListReadFiles(ctx, sessionID)
}

// -- History --

func (w *AppWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return w.app.History.ListBySession(ctx, sessionID)
}

// -- LSP --

func (w *AppWorkspace) LSPStart(ctx context.Context, path string) {
	w.app.LSPManager.Start(ctx, path)
}

func (w *AppWorkspace) LSPStopAll(ctx context.Context) {
	w.app.LSPManager.StopAll(ctx)
}

func (w *AppWorkspace) LSPGetStates() map[string]LSPClientInfo {
	states := app.GetLSPStates()
	result := make(map[string]LSPClientInfo, len(states))
	for k, v := range states {
		result[k] = LSPClientInfo{
			Name:            v.Name,
			State:           v.State,
			Error:           v.Error,
			DiagnosticCount: v.DiagnosticCount,
			ConnectedAt:     v.ConnectedAt,
		}
	}
	return result
}

func (w *AppWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	state, ok := app.GetLSPState(name)
	if !ok || state.Client == nil {
		return lsp.DiagnosticCounts{}
	}
	return state.Client.GetDiagnosticCounts()
}

// -- Config (read-only) --

func (w *AppWorkspace) Config() *config.Config {
	return w.store.Config()
}

func (w *AppWorkspace) WorkingDir() string {
	return w.store.WorkingDir()
}

func (w *AppWorkspace) Resolver() config.VariableResolver {
	return w.store.Resolver()
}

// -- Config mutations --

func (w *AppWorkspace) UpdatePreferredModel(scope config.Scope, modelType config.SelectedModelType, model config.SelectedModel) error {
	return w.store.UpdatePreferredModel(scope, modelType, model)
}

func (w *AppWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	return w.store.SetCompactMode(scope, enabled)
}

func (w *AppWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	return w.store.SetProviderAPIKey(scope, providerID, apiKey)
}

func (w *AppWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	return w.store.SetConfigField(scope, key, value)
}

func (w *AppWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	return w.store.RemoveConfigField(scope, key)
}

func (w *AppWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return w.store.ImportCopilot()
}

func (w *AppWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return w.store.RefreshOAuthToken(ctx, scope, providerID)
}

// -- Project lifecycle --

func (w *AppWorkspace) ProjectNeedsInitialization() (bool, error) {
	return config.ProjectNeedsInitialization(w.store)
}

func (w *AppWorkspace) MarkProjectInitialized() error {
	return config.MarkProjectInitialized(w.store)
}

func (w *AppWorkspace) InitializePrompt() (string, error) {
	return agent.InitializePrompt(w.store)
}

func (w *AppWorkspace) ListSkills(_ context.Context) ([]skills.CatalogEntry, error) {
	mgr := w.app.Skills
	return skills.Catalog(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir()), nil
}

func (w *AppWorkspace) ReadSkill(_ context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	mgr := w.app.Skills
	return skills.ReadContent(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir(), skillID)
}

// -- MCP operations --

func (w *AppWorkspace) MCPGetStates() map[string]mcptools.ClientInfo {
	return mcptools.GetStates()
}

func (w *AppWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	mcptools.RefreshPrompts(ctx, name)
}

func (w *AppWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	mcptools.RefreshResources(ctx, name)
}

func (w *AppWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	mcptools.RefreshTools(ctx, w.store, name)
}

func (w *AppWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	contents, err := mcptools.ReadResource(ctx, w.store, name, uri)
	if err != nil {
		return nil, err
	}
	result := make([]MCPResourceContents, len(contents))
	for i, c := range contents {
		result[i] = MCPResourceContents{
			URI:      c.URI,
			MIMEType: c.MIMEType,
			Text:     c.Text,
			Blob:     c.Blob,
		}
	}
	return result, nil
}

func (w *AppWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return commands.GetMCPPrompt(w.store, clientID, promptID, args)
}

func (w *AppWorkspace) EnableDockerMCP(ctx context.Context) error {
	mcpConfig, err := w.store.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}

	if err := mcptools.InitializeSingle(ctx, config.DockerMCPName, w.store); err != nil {
		disableErr := mcptools.DisableSingle(w.store, config.DockerMCPName)
		delete(w.store.Config().MCP, config.DockerMCPName)
		return fmt.Errorf("failed to start docker MCP: %w", errors.Join(err, disableErr))
	}

	if err := w.store.PersistDockerMCPConfig(mcpConfig); err != nil {
		disableErr := mcptools.DisableSingle(w.store, config.DockerMCPName)
		delete(w.store.Config().MCP, config.DockerMCPName)
		return fmt.Errorf("docker MCP started but failed to persist configuration: %w", errors.Join(err, disableErr))
	}

	return nil
}

func (w *AppWorkspace) DisableDockerMCP() error {
	if err := mcptools.DisableSingle(w.store, config.DockerMCPName); err != nil {
		return fmt.Errorf("failed to disable docker MCP: %w", err)
	}
	return w.store.DisableDockerMCP()
}

// -- Lifecycle --

func (w *AppWorkspace) Subscribe(program *tea.Program) {
	w.app.Subscribe(program)
}

func (w *AppWorkspace) Shutdown() {
	w.app.Shutdown()
}

// App returns the underlying app.App instance.
func (w *AppWorkspace) App() *app.App {
	return w.app
}

// Store returns the underlying config store.
func (w *AppWorkspace) Store() *config.ConfigStore {
	return w.store
}

// Compile-time check that AppWorkspace implements Workspace.
var _ Workspace = (*AppWorkspace)(nil)
