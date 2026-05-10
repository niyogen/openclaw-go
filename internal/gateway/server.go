package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/config"
	"openclaw-go/internal/cronstore"
	"openclaw-go/internal/hookstore"
	"openclaw-go/internal/logstore"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/runtime"
	"openclaw-go/internal/secretstore"
	"openclaw-go/internal/sessions"
	"openclaw-go/internal/topology"

	"os"
	"path/filepath"

	"github.com/gorilla/websocket"
)

type Server struct {
	host            string
	port            int
	authToken       string
	password        string
	trustedProxies  []string
	allowedOrigins  map[string]struct{}
	store           *sessions.Store
	runner          agents.Runner
	route           *channels.Router
	registry        *plugins.Registry
	tools           *ToolRegistry
	approvals       *runtime.ApprovalQueue
	logs            *logstore.Store
	cron            *cronstore.Store
	hooks           *hookstore.Store
	secrets         *secretstore.Store
	rateLimiter     *RateLimiter
	bus             *EventBus
	shutdownMu      sync.Mutex
	shutdownFn      func()
	topo            *topology.Store
	workspace       *agents.Workspace
	mux                    *http.ServeMux
	startedAt              time.Time
	shutdownTimeout        time.Duration
	defaultMaxContextMsgs  int
	runnerFactory          func(provider, model string) agents.Runner
	runnerCache            map[string]agents.Runner
	runnerCacheMu          sync.Mutex
}

func New(
	host string,
	port int,
	authToken string,
	allowedOrigins []string,
	store *sessions.Store,
	runner agents.Runner,
	router *channels.Router,
	registry *plugins.Registry,
	dataDir string,
) *Server {
	mux := http.NewServeMux()
	s := &Server{
		host:           host,
		port:           port,
		authToken:      strings.TrimSpace(authToken),
		allowedOrigins: normalizeOrigins(allowedOrigins),
		store:          store,
		runner:         runner,
		route:          router,
		registry:       registry,
		mux:            mux,
		startedAt:      time.Now().UTC(),
	}
	if s.route == nil {
		s.route = channels.NewRouter()
	}
	s.approvals = runtime.NewApprovalQueue()
	s.rateLimiter = NewRateLimiter(120, time.Minute)
	s.bus = NewEventBus()
	s.shutdownFn = func() {} // replaced by Run()
	// Use filepath.Join for all paths to ensure correct separators on Windows.
	// Log a warning to stderr when primary dataDir stores fail so operators
	// know they're falling back to volatile tmpdir-backed state.
	fallbackWarn := func(name, tmpPath string) {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: %s store unavailable in %s — falling back to %s (data will not survive restart)\n", name, dataDir, tmpPath)
	}
	s.topo, _ = topology.New(filepath.Join(dataDir, "topology.json"))
	s.workspace, _ = agents.NewWorkspace(filepath.Join(dataDir, "workspace.json"))
	if s.topo == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-topology.json")
		fallbackWarn("topology", tmp)
		s.topo, _ = topology.New(tmp)
	}
	if s.workspace == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-workspace.json")
		fallbackWarn("workspace", tmp)
		s.workspace, _ = agents.NewWorkspace(tmp)
	}
	s.logs, _ = logstore.New(filepath.Join(dataDir, "logs.json"))
	s.cron, _ = cronstore.New(filepath.Join(dataDir, "cron.json"))
	s.hooks, _ = hookstore.New(filepath.Join(dataDir, "hooks.json"))
	s.secrets, _ = secretstore.New(filepath.Join(dataDir, "secrets.json"))
	if s.logs == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-logs.json")
		fallbackWarn("logs", tmp)
		s.logs, _ = logstore.New(tmp)
	}
	if s.cron == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-cron.json")
		fallbackWarn("cron", tmp)
		s.cron, _ = cronstore.New(tmp)
	}
	if s.hooks == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-hooks.json")
		fallbackWarn("hooks", tmp)
		s.hooks, _ = hookstore.New(tmp)
	}
	if s.secrets == nil {
		tmp := filepath.Join(os.TempDir(), "openclaw-go-secrets.json")
		fallbackWarn("secrets", tmp)
		s.secrets, _ = secretstore.New(tmp)
	}
	s.shutdownTimeout = 5 * time.Second
	s.runnerCache = map[string]agents.Runner{}
	s.initTools()
	s.registerRoutes()
	s.registerOpenAICompatRoutes()
	s.registerUIRoutes()
	registry.MountRoutes(mux)
	return s
}

// SetShutdownTimeout overrides the graceful shutdown drain period (default 5s).
func (s *Server) SetShutdownTimeout(d time.Duration) {
	if d > 0 {
		s.shutdownTimeout = d
	}
}

// SetDefaultMaxContextMessages sets the server-wide default for history
// truncation. Per-request policy.MaxContextMessages overrides this.
func (s *Server) SetDefaultMaxContextMessages(n int) {
	if n >= 0 {
		s.defaultMaxContextMsgs = n
	}
}

// SetRunnerFactory installs a factory used to create per-session runners when
// a session has a specific provider/model override set.
func (s *Server) SetRunnerFactory(fn func(provider, model string) agents.Runner) {
	s.runnerFactory = fn
}

// runnerForSession returns a session-specific Runner when the session has a
// provider/model override, otherwise returns the global s.runner.
// The cache lookup and session read happen under the same lock to avoid
// TOCTOU races where SetSessionModel changes the model between the Get and
// the cache insert.
func (s *Server) runnerForSession(sessionID string) agents.Runner {
	if s.runnerFactory == nil {
		return s.runner
	}
	s.runnerCacheMu.Lock()
	defer s.runnerCacheMu.Unlock()

	sess, ok := s.store.Get(sessionID)
	if !ok || (sess.Provider == "" && sess.Model == "") {
		return s.runner
	}
	key := sess.Provider + ":" + sess.Model
	if r, exists := s.runnerCache[key]; exists {
		return r
	}
	r := s.runnerFactory(sess.Provider, sess.Model)
	if r == nil {
		return s.runner // factory returned nil — fall back to default
	}
	s.runnerCache[key] = r
	return r
}

func (s *Server) Address() string {
	return fmt.Sprintf("%s:%d", s.host, s.port)
}

// Bus returns the internal event bus so callers can subscribe to gateway events.
func (s *Server) Bus() *EventBus { return s.bus }

// SetAuth configures additional auth modes (password, trusted proxies).
// Safe to call after Run() for hot reload via SIGHUP.
func (s *Server) SetAuth(password string, trustedProxies []string) {
	s.password = strings.TrimSpace(password)
	s.trustedProxies = trustedProxies
}

// SetAuthToken replaces the bearer token. Safe to call after Run().
func (s *Server) SetAuthToken(token string) {
	s.authToken = strings.TrimSpace(token)
}

// SetAllowedOrigins replaces the CORS/WS origin allowlist. Safe to call after Run().
func (s *Server) SetAllowedOrigins(origins []string) {
	s.allowedOrigins = normalizeOrigins(origins)
}

// RegisterExternalPlugin registers an external plugin with the gateway at
// runtime (after New()).  The plugin's routes are mounted on the mux and it
// is added to the plugin registry so /plugins lists it.
func (s *Server) RegisterExternalPlugin(ep plugins.Plugin) {
	s.registry.Register(ep)
	ep.RegisterRoutes(s.mux)
}

// RegisterPluginWS registers a WebSocket upgrade path for an external plugin.
// Frames sent to /ws/plugins/<name> are forwarded to the plugin's WS endpoint.
func (s *Server) RegisterPluginWS(pluginName, targetWSURL string) {
	path := "/ws/plugins/" + pluginName
	s.mux.HandleFunc(path, s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(req *http.Request) bool {
				return s.isAllowedOrigin(req.Header.Get("Origin"))
			},
		}
		clientConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer clientConn.Close()

		// Connect to plugin backend.
		pluginConn, dialResp, err := websocket.DefaultDialer.DialContext(r.Context(), targetWSURL, nil)
		if dialResp != nil {
			dialResp.Body.Close()
		}
		if err != nil {
			_ = clientConn.WriteJSON(map[string]string{"error": "plugin ws unavailable: " + err.Error()})
			return
		}
		defer pluginConn.Close()

		// Bidirectional proxy.
		done := make(chan struct{}, 2)
		forward := func(from, to *websocket.Conn) {
			defer func() { done <- struct{}{} }()
			for {
				msgType, msg, err := from.ReadMessage()
				if err != nil {
					return
				}
				if err := to.WriteMessage(msgType, msg); err != nil {
					return
				}
			}
		}
		go forward(clientConn, pluginConn)
		go forward(pluginConn, clientConn)
		<-done
	}))
}

func (s *Server) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if pattern == "" || handler == nil {
		return
	}
	s.mux.HandleFunc(pattern, handler)
}

func (s *Server) Run(ctx context.Context) error {
	// Wire shutdown function so gateway.stop RPC works.
	ctx, cancelFromRPC := context.WithCancel(ctx)
	defer cancelFromRPC()
	s.shutdownMu.Lock()
	s.shutdownFn = cancelFromRPC
	s.shutdownMu.Unlock()

	// Wrap every request with trace logging.
	tracedMux := s.withTrace(s.mux)

	// Start cron scheduler — jobs fire on their schedule and commands are executed.
	s.cron.StartScheduler(ctx, func(cronCtx context.Context, job cronstore.Job) {
		s.logs.Append(logstore.LevelInfo, "cron", "job fired: "+job.Name, map[string]any{"id": job.ID, "schedule": job.Schedule})
		run := s.cron.ExecuteJob(cronCtx, job)
		s.bus.Publish(GatewayEvent{Type: EventToolInvoked, Data: map[string]any{
			"cron": job.ID, "command": job.Command, "exitCode": run.ExitCode, "output": run.Output,
		}})
		s.hooks.Emit(hookstore.EventToolInvoked, map[string]any{"cron": job.ID, "command": job.Command})
	})

	server := &http.Server{
		Addr:    s.Address(),
		Handler: tracedMux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) registerRoutes() {
	cors := s.withCORSMiddleware
	s.mux.HandleFunc("/health", cors(s.handleHealth))
	s.mux.Handle("GET /tools", cors(s.withAuth(s.handleToolsList)))
	s.mux.Handle("POST /tools/invoke", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleToolsInvoke)))))
	s.mux.Handle("GET /sessions", cors(s.withAuth(s.handleListSessions)))
	s.mux.Handle("GET /sessions/{id}", cors(s.withAuth(s.handleGetSession)))
	s.mux.Handle("DELETE /sessions/{id}", cors(s.withAuth(s.handleDeleteSession)))
	s.mux.Handle("DELETE /sessions", cors(s.withAuth(withBodyLimit(s.handleBulkDeleteSessions))))
	s.mux.Handle("GET /sessions/{id}/history", cors(s.withAuth(s.handleSessionHistory)))
	s.mux.Handle("POST /sessions/{id}/patch", cors(s.withAuth(withBodyLimit(s.handleSessionPatch))))
	s.mux.Handle("POST /sessions/{id}/kill", cors(s.withAuth(s.handleSessionKill)))
	s.mux.Handle("POST /sessions/{id}/compact", cors(s.withAuth(s.handleSessionCompact)))
	s.mux.Handle("GET /sessions/{id}/stats", cors(s.withAuth(s.handleSessionStats)))
	s.mux.Handle("POST /sessions/{id}/model", cors(s.withAuth(withBodyLimit(s.handleSessionSetModel))))
	s.mux.Handle("GET /agent/run/{runId}", cors(s.withAuth(s.handleAgentRunGet)))
	s.mux.HandleFunc("/message", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleMessage)))))
	s.mux.Handle("POST /agent/run", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleAgentRun)))))
	s.mux.Handle("POST /agent/run/stream", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleAgentRunStream)))))
	s.mux.Handle("GET /approvals", cors(s.withAuth(s.handleApprovalsList)))
	s.mux.Handle("POST /approvals/{id}/decide", cors(s.withAuth(withBodyLimit(s.handleApprovalDecide))))
	s.mux.Handle("GET /logs", cors(s.withAuth(s.handleLogsList)))
	s.mux.Handle("GET /cron", cors(s.withAuth(s.handleCronList)))
	s.mux.Handle("POST /cron", cors(s.withAuth(withBodyLimit(s.handleCronAdd))))
	s.mux.Handle("DELETE /cron/{id}", cors(s.withAuth(s.handleCronDelete)))
	s.mux.Handle("GET /hooks", cors(s.withAuth(s.handleHooksList)))
	s.mux.Handle("POST /hooks", cors(s.withAuth(withBodyLimit(s.handleHooksAdd))))
	s.mux.Handle("DELETE /hooks/{id}", cors(s.withAuth(s.handleHooksDelete)))
	s.mux.Handle("GET /secrets", cors(s.withAuth(s.handleSecretsList)))
	s.mux.Handle("POST /secrets", cors(s.withAuth(withBodyLimit(s.handleSecretsSet))))
	s.mux.Handle("DELETE /secrets/{name}", cors(s.withAuth(s.handleSecretsDelete)))
	s.mux.HandleFunc("/rpc", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleRPC)))))
	s.mux.HandleFunc("/ws", s.withAuth(s.handleWS))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Liveness probe: gateway process is alive.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "live",
		"service": "openclaw-go-gateway",
		"version": Version,
		"time":    time.Now().UTC(),
	})
}

// sessionSummary is a projection of Session omitting the full message history,
// used in list responses to avoid transmitting unbounded message payloads.
type sessionSummary struct {
	ID           string    `json:"id"`
	Channel      string    `json:"channel"`
	Target       string    `json:"target"`
	MessageCount int       `json:"messageCount"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filterChannel := q.Get("channel")
	filterSince := q.Get("since")
	cursor := q.Get("cursor")
	limit := 50
	if l := q.Get("limit"); l != "" {
		var n int
		if cnt, _ := fmt.Sscanf(l, "%d", &n); cnt == 1 && n > 0 && n <= 500 {
			limit = n
		}
	}

	var sinceTime time.Time
	if filterSince != "" {
		sinceTime, _ = time.Parse(time.RFC3339, filterSince)
	}

	all := s.store.List()

	// Stable sort: UpdatedAt descending, ID ascending as tiebreaker.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].UpdatedAt.Equal(all[j].UpdatedAt) {
			return all[i].ID < all[j].ID
		}
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})

	// Apply cursor — if the cursor ID is not found return empty page rather
	// than silently restarting at page one.
	start := 0
	if cursor != "" {
		found := false
		for i, sess := range all {
			if sess.ID == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusOK, map[string]any{"sessions": []sessionSummary{}, "total": s.store.Count()})
			return
		}
	}

	summaries := make([]sessionSummary, 0, limit)
	var nextCursor string
	for i := start; i < len(all); i++ {
		sess := all[i]
		if filterChannel != "" && sess.Channel != filterChannel {
			continue
		}
		if !sinceTime.IsZero() && sess.UpdatedAt.Before(sinceTime) {
			continue
		}
		if len(summaries) >= limit {
			nextCursor = sess.ID
			break
		}
		summaries = append(summaries, sessionSummary{
			ID:           sess.ID,
			Channel:      sess.Channel,
			Target:       sess.Target,
			MessageCount: len(sess.Messages),
			UpdatedAt:    sess.UpdatedAt,
		})
	}

	resp := map[string]any{"sessions": summaries, "total": s.store.Count()}
	if nextCursor != "" {
		resp["nextCursor"] = nextCursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	sess, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	history, ok := s.store.History(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": id, "history": history})
}

func (s *Server) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	var patches []sessions.MessagePatch
	if err := json.NewDecoder(r.Body).Decode(&patches); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if err := s.store.Patch(id, patches); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "patched": len(patches)})
}

func (s *Server) handleSessionCompact(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	var req struct {
		KeepN int `json:"keepN"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.KeepN <= 0 {
		req.KeepN = 20 // default: keep last 20 messages
	}
	removed, err := s.store.Compact(id, req.KeepN)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

func (s *Server) handleSessionStats(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	stats, ok := s.store.Stats(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleSessionSetModel sets a per-session model/provider override.
// Body: {"provider":"openai","model":"gpt-4o"}
func (s *Server) handleSessionSetModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if err := s.store.SetSessionModel(id, req.Provider, req.Model); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Evict cached runner so next request gets a fresh one with new model.
	s.runnerCacheMu.Lock()
	delete(s.runnerCache, req.Provider+":"+req.Model)
	s.runnerCacheMu.Unlock()
	_ = s.logs.Append(logstore.LevelInfo, "sessions", "session model set: "+id, //nolint:errcheck
		map[string]any{"provider": req.Provider, "model": req.Model})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionId": id, "provider": req.Provider, "model": req.Model})
}

func (s *Server) handleSessionKill(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if err := s.store.Kill(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.bus.Publish(GatewayEvent{Type: EventSessionKilled, SessionID: id})
	s.logs.Append(logstore.LevelInfo, "sessions", "session killed: "+id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "killed": id})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	deleted, err := s.store.Delete(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	s.bus.Publish(GatewayEvent{Type: EventSessionDeleted, SessionID: id})
	s.logs.Append(logstore.LevelInfo, "sessions", "session deleted: "+id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

func (s *Server) handleToolsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": s.tools.List(),
	})
}

func (s *Server) handleToolsInvoke(w http.ResponseWriter, r *http.Request) {
	var req ToolInvokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	result, err := s.tools.Invoke(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"result": result,
	})
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	if next == nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) isAuthorized(r *http.Request) bool {
	// If no auth configured, allow all.
	if strings.TrimSpace(s.authToken) == "" && strings.TrimSpace(s.password) == "" {
		return true
	}

	authorization := strings.TrimSpace(r.Header.Get("Authorization"))

	// Bearer token.
	if s.authToken != "" {
		if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			token := strings.TrimSpace(authorization[len("Bearer "):])
			if token == s.authToken {
				return true
			}
		}
		headerToken := strings.TrimSpace(r.Header.Get("X-OpenClaw-Token"))
		if headerToken != "" && headerToken == s.authToken {
			return true
		}
		queryToken := strings.TrimSpace(r.URL.Query().Get("token"))
		if queryToken != "" && queryToken == s.authToken {
			return true
		}
	}

	// HTTP Basic password auth.
	if s.password != "" {
		if _, pass, ok := r.BasicAuth(); ok && pass == s.password {
			return true
		}
	}

	// Trusted proxy: allow requests from configured proxy IPs/CIDRs without auth.
	if len(s.trustedProxies) > 0 {
		remoteIP := clientIP(r)
		if isTrustedProxy(remoteIP, s.trustedProxies) {
			return true
		}
	}

	return false
}

type messageRequest struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionId is required"})
		return
	}
	if req.Channel == "" {
		req.Channel = "cli"
	}

	reply, err := s.processMessage(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": req.SessionID,
		"reply":     reply,
	})
}

func (s *Server) processMessage(ctx context.Context, req messageRequest) (string, error) {
	created := false
	if _, exists := s.store.Get(req.SessionID); !exists {
		created = true
	}
	if err := s.store.UpsertSession(req.SessionID, req.Channel, req.Target); err != nil {
		return "", err
	}
	if created {
		s.hooks.Emit(hookstore.EventSessionCreated, map[string]any{
			"sessionId": req.SessionID, "channel": req.Channel,
		})
		s.bus.Publish(GatewayEvent{Type: EventSessionCreated, SessionID: req.SessionID})
		s.logs.Append(logstore.LevelInfo, "sessions", "session created: "+req.SessionID, nil)
	}
	// Snapshot history BEFORE appending the current user message so that
	// buildMessages (which also appends turn.Message) does not duplicate it.
	historyMessages := []agents.HistoryMessage{}
	if existing, ok := s.store.Get(req.SessionID); ok {
		for _, msg := range existing.Messages {
			historyMessages = append(historyMessages, agents.HistoryMessage{
				Role:    string(msg.Role),
				Content: msg.Content,
			})
		}
	}
	if err := s.store.AppendMessage(req.SessionID, sessions.Message{
		Role:      sessions.RoleUser,
		Content:   req.Message,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	s.hooks.Emit(hookstore.EventMessageReceived, map[string]any{
		"sessionId": req.SessionID, "channel": req.Channel, "message": req.Message,
	})
	s.bus.Publish(GatewayEvent{
		Type:      EventSessionMessage,
		SessionID: req.SessionID,
		Data:      map[string]any{"role": "user", "content": req.Message},
	})
	// Truncate history if a context window limit is configured.
	if s.defaultMaxContextMsgs > 0 {
		historyMessages = runtime.TruncateHistory(historyMessages, s.defaultMaxContextMsgs)
	}
	reply, err := s.runnerForSession(req.SessionID).GenerateReply(ctx, agents.Turn{
		Message: req.Message,
		History: historyMessages,
	})
	if err != nil {
		return "", err
	}
	if err := s.store.AppendMessage(req.SessionID, sessions.Message{
		Role:      sessions.RoleAssistant,
		Content:   reply,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	if dispatchErr := s.route.Dispatch(ctx, channels.OutboundMessage{
		SessionID: req.SessionID,
		Channel:   req.Channel,
		Target:    req.Target,
		Message:   reply,
	}); dispatchErr != nil {
		s.logs.Append(logstore.LevelWarn, "channels", //nolint:errcheck
			"outbound dispatch failed: "+dispatchErr.Error(),
			map[string]any{"sessionId": req.SessionID, "channel": req.Channel})
	}
	s.hooks.Emit(hookstore.EventMessageSent, map[string]any{
		"sessionId": req.SessionID, "channel": req.Channel, "reply": reply,
	})
	s.bus.Publish(GatewayEvent{
		Type:      EventSessionMessage,
		SessionID: req.SessionID,
		Data:      map[string]any{"role": "assistant", "content": reply},
	})
	s.bus.Publish(GatewayEvent{
		Type:      EventAgentReply,
		SessionID: req.SessionID,
		Data:      map[string]any{"reply": reply},
	})
	s.logs.Append(logstore.LevelInfo, "message", "reply sent for "+req.SessionID, nil)
	return reply, nil
}

func (s *Server) HandleInbound(ctx context.Context, inbound channels.InboundMessage) (string, error) {
	sessionID := inbound.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("%s:%s", inbound.Channel, inbound.Target)
	}
	req := messageRequest{
		SessionID: sessionID,
		Channel:   inbound.Channel,
		Target:    inbound.Target,
		Message:   inbound.Message,
	}
	return s.processMessage(ctx, req)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &rpcError{Code: -32700, Message: "parse error"},
		})
		return
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}
	if req.JSONRPC != "2.0" {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32600, Message: "invalid request"},
		})
		return
	}
	result, rpcErr := s.dispatchRPC(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   rpcErr,
		})
		return
	}
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

func (s *Server) gatewayStatus() map[string]any {
	return map[string]any{
		"ok":          true,
		"service":     "openclaw-go-gateway",
		"version":     Version,
		"address":     s.Address(),
		"authEnabled": strings.TrimSpace(s.authToken) != "" || strings.TrimSpace(s.password) != "",
		"uptime":      time.Since(s.startedAt).String(),
		"time":        time.Now().UTC(),
	}
}

type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

func (s *Server) dispatchRPC(
	ctx context.Context,
	method string,
	params json.RawMessage,
) (any, *rpcError) {
	switch method {
	case "health":
		return map[string]any{
			"ok":      true,
			"service": "openclaw-go-gateway",
			"version": Version,
			"time":    time.Now().UTC(),
		}, nil
	case "gateway.status", "status":
		return s.gatewayStatus(), nil
	case "sessions.list":
		all := s.store.List()
		summaries := make([]sessionSummary, 0, len(all))
		for _, sess := range all {
			summaries = append(summaries, sessionSummary{
				ID:           sess.ID,
				Channel:      sess.Channel,
				Target:       sess.Target,
				MessageCount: len(sess.Messages),
				UpdatedAt:    sess.UpdatedAt,
			})
		}
		return map[string]any{"sessions": summaries}, nil
	case "sessions.get":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		sess, ok := s.store.Get(p.SessionID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return map[string]any{"session": sess}, nil
	case "sessions.history":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		history, ok := s.store.History(p.SessionID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return map[string]any{"sessionId": p.SessionID, "history": history}, nil
	case "sessions.kill":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Kill(p.SessionID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		s.bus.Publish(GatewayEvent{Type: EventSessionKilled, SessionID: p.SessionID})
		s.logs.Append(logstore.LevelInfo, "sessions", "session killed: "+p.SessionID, nil)
		return map[string]any{"ok": true, "killed": p.SessionID}, nil
	case "sessions.delete":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		deleted, err := s.store.Delete(p.SessionID)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !deleted {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		s.bus.Publish(GatewayEvent{Type: EventSessionDeleted, SessionID: p.SessionID})
		s.logs.Append(logstore.LevelInfo, "sessions", "session deleted: "+p.SessionID, nil)
		return map[string]any{"ok": true, "deleted": p.SessionID}, nil
	case "plugins.list":
		if s.registry == nil {
			return map[string]any{"plugins": []string{}}, nil
		}
		return map[string]any{"plugins": s.registry.Names()}, nil
	case "agent.run":
		var req agentRunRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(req.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if strings.TrimSpace(req.Message) == "" {
			return nil, &rpcError{Code: -32602, Message: "message is required"}
		}
		policy := runtime.DefaultPolicy()
		if req.Policy != nil {
			policy = *req.Policy
		}
		_ = s.store.UpsertSession(req.SessionID, "cli", "")
		// Snapshot history BEFORE appending user message (same as handleAgentRun).
		var rpcHistory []agents.HistoryMessage
		if sess, ok := s.store.Get(req.SessionID); ok {
			for _, m := range sess.Messages {
				rpcHistory = append(rpcHistory, agents.HistoryMessage{
					Role:    string(m.Role),
					Content: m.Content,
				})
			}
		}
		_ = s.store.AppendMessage(req.SessionID, sessions.Message{
			Role:      sessions.RoleUser,
			Content:   req.Message,
			CreatedAt: time.Now().UTC(),
		})
		toolFn := func(fctx context.Context, name string, args map[string]any) (any, error) {
			return s.tools.Invoke(fctx, ToolInvokeRequest{Name: name, Arguments: args})
		}
		exec := runtime.NewExecutor(s.runnerForSession(req.SessionID), toolFn)
		exec.SetSubagentFn(func(fctx context.Context, message, instructions string) (string, error) {
			var subHistory []agents.HistoryMessage
			if strings.TrimSpace(instructions) != "" {
				subHistory = []agents.HistoryMessage{{Role: "system", Content: instructions}}
			}
			return s.runnerForSession(req.SessionID).GenerateReply(fctx, agents.Turn{Message: message, History: subHistory})
		})
		result := exec.Run(ctx, runtime.RunOptions{
			SessionID:    req.SessionID,
			Message:      req.Message,
			History:      rpcHistory,
			Instructions: req.Instructions,
			Policy:       policy,
			Approvals:    s.approvals,
		})
		var errStr string
		if result.Err != nil {
			errStr = result.Err.Error()
		} else if result.FinalText != "" {
			_ = s.store.AppendMessage(req.SessionID, sessions.Message{
				Role:      sessions.RoleAssistant,
				Content:   result.FinalText,
				CreatedAt: time.Now().UTC(),
			})
		}
		runID := generateRunID()
		globalRunStore.put(runID, result)
		return map[string]any{
			"runId":     runID,
			"sessionId": req.SessionID,
			"reply":     result.FinalText,
			"turns":     len(result.Turns),
			"error":     errStr,
		}, nil
	case "approvals.list":
		return map[string]any{"approvals": s.approvals.List()}, nil
	case "approvals.decide":
		var p struct {
			ID       string `json:"id"`
			Approved bool   `json:"approved"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if err := s.approvals.Decide(p.ID, p.Approved); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": p.ID, "approved": p.Approved}, nil
	case "sessions.subscribe":
		// Over JSON-RPC / REST, return recent events from the bus (poll-style).
		// For real push, use the WS frame type "sessions.subscribe" instead.
		var p sessionIDParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
		}
		evCh, unsub := s.bus.Subscribe(p.SessionID)
		defer unsub()
		// Collect up to 100ms worth of buffered events.
		deadline := time.After(100 * time.Millisecond)
		var events []GatewayEvent
		for {
			select {
			case ev, ok := <-evCh:
				if !ok {
					goto done
				}
				events = append(events, ev)
			case <-deadline:
				goto done
			case <-ctx.Done():
				goto done
			}
		}
	done:
		return map[string]any{"events": events}, nil
	case "logs.list":
		return s.rpcLogs(params)
	case "cron.list":
		return s.rpcCronList()
	case "cron.run":
		// One-shot manual execution of a cron job by id.
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		job, ok := s.cron.Get(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "cron job not found"}
		}
		if !s.cron.TryLockRunning(p.ID) {
			return nil, &rpcError{Code: -32000, Message: "job is already running"}
		}
		go func() {
			defer s.cron.UnlockRunning(p.ID)
			s.cron.ExecuteJob(context.Background(), job)
		}()
		return map[string]any{"ok": true, "id": p.ID, "message": "job triggered"}, nil
	case "cron.runs":
		var p struct {
			ID    string `json:"id"`
			Limit int    `json:"limit"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if p.Limit <= 0 {
			p.Limit = 50
		}
		return map[string]any{"runs": s.cron.Runs(p.ID, p.Limit)}, nil
	case "cron.update":
		var job cronstore.Job
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &job); err != nil || strings.TrimSpace(job.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "job id is required"}
		}
		if err := s.cron.Add(job); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": job.ID}, nil
	case "cron.status":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		job, ok := s.cron.Get(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "job not found"}
		}
		return map[string]any{
			"job":      job,
			"lastRuns": s.cron.Runs(p.ID, 10),
		}, nil
	case "cron.add":
		return s.rpcCronAdd(params)
	case "cron.delete":
		return s.rpcCronDelete(params)
	case "hooks.list":
		return s.rpcHooksList()
	case "hooks.add":
		return s.rpcHooksAdd(params)
	case "hooks.delete":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		deleted, err := s.hooks.Remove(p.ID)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !deleted {
			return nil, &rpcError{Code: -32001, Message: "hook not found"}
		}
		return map[string]any{"ok": true, "deleted": p.ID}, nil
	case "sessions.compact":
		var p struct {
			SessionID string `json:"sessionId"`
			KeepN     int    `json:"keepN"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if p.KeepN <= 0 {
			p.KeepN = 20
		}
		removed, err := s.store.Compact(p.SessionID, p.KeepN)
		if err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "removed": removed}, nil
	case "sessions.stats":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		stats, ok := s.store.Stats(p.SessionID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return stats, nil
	case "sessions.patch":
		var p struct {
			SessionID string                  `json:"sessionId"`
			Patches   []sessions.MessagePatch `json:"patches"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Patch(p.SessionID, p.Patches); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "patched": len(p.Patches)}, nil
	case "secrets.list":
		return s.rpcSecretsList()
	case "secrets.set":
		return s.rpcSecretsSet(params)
	case "secrets.get":
		return s.rpcSecretsGet(params)
	case "secrets.delete":
		return s.rpcSecretsDelete(params)
	case "models.list":
		var providerFilter string
		if len(params) > 0 {
			var p struct {
				Provider string `json:"provider"`
			}
			_ = json.Unmarshal(params, &p)
			providerFilter = p.Provider
		}
		if strings.TrimSpace(providerFilter) == "" {
			return map[string]any{"models": agents.KnownModels()}, nil
		}
		return map[string]any{"models": agents.ModelsForProvider(providerFilter)}, nil
	case "models.capability":
		var p struct {
			Provider string `json:"provider"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if strings.TrimSpace(p.Provider) == "" {
			p.Provider = "echo"
		}
		return agents.Capability(p.Provider, ""), nil
	case "tools.list":
		return map[string]any{"tools": s.tools.List()}, nil
	case "tools.invoke":
		var req ToolInvokeRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		result, err := s.tools.Invoke(ctx, req)
		if err != nil {
			return nil, &rpcError{Code: -32002, Message: err.Error()}
		}
		return map[string]any{"result": result}, nil
	case "message.send":
		var req messageRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if req.SessionID == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if req.Channel == "" {
			req.Channel = "cli"
		}
		reply, err := s.processMessage(ctx, req)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{
			"sessionId": req.SessionID,
			"reply":     reply,
		}, nil

	// ── config.* ───────────────────────────────────────────────────────────
	case "config.get":
		return map[string]any{
			"gateway": map[string]any{
				"host":        s.host,
				"address":     s.Address(),
				"version":     Version,
				"authEnabled": strings.TrimSpace(s.authToken) != "" || strings.TrimSpace(s.password) != "",
			},
			"tools":    s.tools.List(),
			"plugins":  s.registry.Names(),
			"channels": s.route.Names(),
		}, nil
	case "config.schema":
		// Return field names and types for the Config struct.
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"gateway":   map[string]string{"type": "object"},
				"agent":     map[string]string{"type": "object"},
				"providers": map[string]string{"type": "object"},
				"channels":  map[string]string{"type": "object"},
				"memory":    map[string]string{"type": "object"},
				"mcp":       map[string]string{"type": "array"},
				"skills":    map[string]string{"type": "array"},
				"nodes":     map[string]string{"type": "array"},
			},
		}, nil
	case "config.schema.lookup":
		var p struct {
			Key string `json:"key"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		// Simplified lookup — returns description for known keys.
		descriptions := map[string]string{
			"gateway.authToken":         "Bearer token for gateway auth",
			"gateway.host":              "Gateway bind host",
			"gateway.port":              "Gateway bind port",
			"agent.provider":            "Model provider: echo, openai, anthropic",
			"channels.telegram.enabled": "Enable Telegram channel",
		}
		if desc, ok := descriptions[p.Key]; ok {
			return map[string]any{"key": p.Key, "description": desc}, nil
		}
		return map[string]any{"key": p.Key, "description": "no description available"}, nil
	case "config.apply", "config.set", "config.patch":
		var patch map[string]any
		if len(params) > 0 {
			if err := json.Unmarshal(params, &patch); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
		}
		// Apply known fields to in-memory state.
		if tok, ok := patch["authToken"].(string); ok {
			s.authToken = strings.TrimSpace(tok)
		}
		if password, ok := patch["password"].(string); ok {
			s.password = strings.TrimSpace(password)
		}
		// Persist to config file so changes survive restart.
		if cfgPath, err := config.DefaultPath(); err == nil {
			if cfg, err := config.Load(cfgPath); err == nil {
				if tok, ok := patch["authToken"].(string); ok {
					cfg.Gateway.AuthToken = strings.TrimSpace(tok)
				}
				if password, ok := patch["password"].(string); ok {
					cfg.Gateway.Password = strings.TrimSpace(password)
				}
				_ = config.Save(cfgPath, cfg)
			}
		}
		return map[string]any{"ok": true, "applied": len(patch)}, nil

	// ── doctor.* ───────────────────────────────────────────────────────────
	case "doctor.check":
		checks := map[string]any{}
		checks["sessionStore"] = map[string]any{"ok": true, "sessions": s.store.Count()}
		checks["runner"] = map[string]any{"ok": s.runner != nil}
		checks["eventBus"] = map[string]any{"ok": s.bus != nil}
		checks["rateLimiter"] = map[string]any{"ok": s.rateLimiter != nil}
		return map[string]any{"ok": true, "checks": checks, "version": Version}, nil

	// ── tools.catalog (alias for tools.list with more detail) ──────────────
	case "tools.catalog":
		return map[string]any{"tools": s.tools.List(), "count": len(s.tools.List())}, nil

	// ── channels.* ─────────────────────────────────────────────────────────
	case "channels.list":
		return map[string]any{"channels": s.route.Names()}, nil
	case "channels.status":
		names := s.route.Names()
		statuses := make([]map[string]any, 0, len(names))
		for _, name := range names {
			statuses = append(statuses, map[string]any{
				"name": name, "status": "active", "enabled": true,
			})
		}
		return map[string]any{"channels": statuses}, nil
	case "channels.start":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "name": p.Name, "status": "started"}, nil
	case "channels.stop":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "name": p.Name, "status": "stopped"}, nil
	case "channels.logout":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "name": p.Name, "status": "logged_out"}, nil

	// ── usage.* ─────────────────────────────────────────────────────────────
	case "usage.stats", "usage.status":
		// Compute aggregate message count without holding full Session payloads.
		sessionList := s.store.List()
		totalMessages := 0
		for _, sess := range sessionList {
			totalMessages += len(sess.Messages)
		}
		return map[string]any{
			"sessions":      s.store.Count(),
			"totalMessages": totalMessages,
			"cronJobs":      len(s.cron.List()),
			"hooks":         len(s.hooks.List()),
			"secrets":       len(s.secrets.List()),
			"plugins":       s.registry.Names(),
		}, nil
	case "usage.cost":
		// Placeholder — real cost tracking requires provider billing APIs.
		return map[string]any{
			"note":          "cost tracking requires provider billing API credentials",
			"totalTokens":   0,
			"estimatedCost": map[string]float64{"usd": 0.0},
		}, nil
	case "tools.effective":
		// tools.effective returns tools visible to the agent after policy filtering.
		return map[string]any{"tools": s.tools.List(), "count": len(s.tools.List())}, nil
	case "secrets.reload":
		// No-op — secrets are loaded on access; return current list metadata.
		return map[string]any{"ok": true, "count": len(s.secrets.List())}, nil
	case "logs.tail":
		var p struct {
			Limit     int    `json:"limit"`
			Level     string `json:"level"`
			Component string `json:"component"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if p.Limit <= 0 {
			p.Limit = 50
		}
		return map[string]any{"logs": s.logs.List(p.Level, p.Component, p.Limit)}, nil
	// ── skills.* ─────────────────────────────────────────────────────────
	case "skills.status":
		return map[string]any{"skills": []any{}, "count": 0, "message": "no skills configured; add skills to ~/.openclaw-go/openclaw.json"}, nil
	case "skills.search":
		var p struct {
			Query string `json:"query"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"results": []any{}, "query": p.Query}, nil
	case "skills.detail":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{
			"name":   p.Name,
			"status": "not_implemented",
			"note":   "skill registry not configured",
		}, nil
	case "skills.bins":
		return map[string]any{"bins": []any{}}, nil
	case "skills.install":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "installed": p.Name, "note": "skill installation requires network access to skill registry"}, nil
	case "skills.update":
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "updated": p.Name}, nil

	// ── update.* ──────────────────────────────────────────────────────────
	case "update.status":
		return map[string]any{"currentVersion": Version, "updateAvailable": false}, nil
	case "update.run":
		return map[string]any{"ok": true, "note": "automated update not implemented; download new binary from releases"}, nil

	// ── doctor.memory.* ──────────────────────────────────────────────────
	case "doctor.memory", "doctor.memory.check":
		sessionList := s.store.List()
		totalMsgs := 0
		for _, sess := range sessionList {
			totalMsgs += len(sess.Messages)
		}
		return map[string]any{
			"ok":            true,
			"sessions":      s.store.Count(),
			"totalMessages": totalMsgs,
			"logEntries":    len(s.logs.List("", "", 0)),
		}, nil
	case "doctor.memory.clear":
		removed, err := s.store.Cleanup(24 * time.Hour)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "removed": removed}, nil

	// ── TTS ──────────────────────────────────────────────────────────────
	case "tts.status":
		return map[string]any{"available": false, "note": "TTS not configured; set provider in config"}, nil
	case "tts.catalog":
		return map[string]any{"voices": []any{}}, nil
	case "tts.convert":
		return nil, &rpcError{Code: -32000, Message: "TTS not available; configure a TTS provider"}

	// ── diagnostics ───────────────────────────────────────────────────────
	case "diagnostics.stability":
		return map[string]any{
			"ok":      true,
			"uptime":  time.Since(s.startedAt).String(),
			"version": Version,
		}, nil

	// ── environments ──────────────────────────────────────────────────────
	case "environments.list":
		return map[string]any{"environments": []string{"default"}}, nil
	case "environments.status":
		return map[string]any{"environment": "default", "status": "active"}, nil

	// ── models auth status ────────────────────────────────────────────────
	case "models.authStatus":
		_, isOpenAI := s.runner.(*agents.OpenAIRunner)
		_, isAnthropic := s.runner.(*agents.AnthropicRunner)
		return map[string]any{
			"openai":    isOpenAI,
			"anthropic": isAnthropic,
		}, nil

	// ── plugins UI descriptors ────────────────────────────────────────────
	case "plugins.uiDescriptors":
		return map[string]any{"descriptors": []any{}}, nil

	// ── agent.* ──────────────────────────────────────────────────────────
	case "agent.identity.get":
		return map[string]any{
			"name":    "openclaw-go-agent",
			"version": Version,
			"provider": func() string {
				if s.runner != nil {
					return "configured"
				}
				return "echo"
			}(),
		}, nil
	case "agent.wait":
		// Long-poll until next agent reply event.
		var p struct {
			SessionID string `json:"sessionId"`
			TimeoutMs int    `json:"timeoutMs"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if p.TimeoutMs <= 0 {
			p.TimeoutMs = 5000
		}
		evCh, unsub := s.bus.Subscribe(p.SessionID)
		defer unsub()
		deadline := time.After(time.Duration(p.TimeoutMs) * time.Millisecond)
		for {
			select {
			case ev := <-evCh:
				if ev.Type == EventAgentReply {
					return map[string]any{"event": ev}, nil
				}
			case <-deadline:
				return map[string]any{"timeout": true}, nil
			case <-ctx.Done():
				return map[string]any{"cancelled": true}, nil
			}
		}

	// ── send (top-level alias) ────────────────────────────────────────────
	case "send":
		var req messageRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(req.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if req.Channel == "" {
			req.Channel = "cli"
		}
		reply, err := s.processMessage(ctx, req)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"reply": reply}, nil

	// ── chat.* ────────────────────────────────────────────────────────────
	case "chat.history":
		var p struct {
			SessionID string `json:"sessionId"`
			Limit     int    `json:"limit"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
		}
		if p.Limit <= 0 {
			p.Limit = 50
		}
		msgs, ok := s.store.Preview(p.SessionID, p.Limit)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return map[string]any{"history": msgs, "sessionId": p.SessionID}, nil
	case "chat.abort":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		_ = s.store.Abort(p.SessionID, "user aborted")
		return map[string]any{"ok": true}, nil
	case "chat.send":
		// Alias for message.send in chat context.
		var req messageRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(req.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if req.Channel == "" {
			req.Channel = "chat"
		}
		reply, err := s.processMessage(ctx, req)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"reply": reply, "sessionId": req.SessionID}, nil

	// ── exec approvals (TS-compatible naming) ─────────────────────────────
	case "exec.approvals.list", "exec.approval.list", "plugin.approval.list":
		return map[string]any{"approvals": s.approvals.List()}, nil
	case "exec.approvals.decide", "exec.approval.decide", "plugin.approval.decide":
		var p struct {
			ID       string `json:"id"`
			Approved bool   `json:"approved"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if err := s.approvals.Decide(p.ID, p.Approved); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": p.ID, "approved": p.Approved}, nil

	// ── node.* ────────────────────────────────────────────────────────────
	case "node.pair.init":
		var p struct {
			NodeID string `json:"nodeId"`
			Name   string `json:"name"`
			URL    string `json:"url"`
			APIKey string `json:"apiKey"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		n := topology.Node{ID: p.NodeID, Name: p.Name, URL: p.URL, APIKey: p.APIKey}
		if err := s.topo.AddNode(n); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "nodeId": n.ID}, nil
	case "node.pair.approve":
		var p struct {
			NodeID string `json:"nodeId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if err := s.topo.UpdateNodeStatus(p.NodeID, topology.NodeStatusOnline); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "nodeId": p.NodeID, "status": "online"}, nil
	case "node.pair.reject":
		var p struct {
			NodeID string `json:"nodeId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		_, _ = s.topo.RemoveNode(p.NodeID)
		return map[string]any{"ok": true, "nodeId": p.NodeID}, nil
	case "node.pending.list":
		nodes := s.topo.ListNodes()
		var pending []topology.Node
		for _, n := range nodes {
			if n.Status == topology.NodeStatusPending {
				pending = append(pending, n)
			}
		}
		return map[string]any{"nodes": pending}, nil
	case "node.invoke":
		// Forward an RPC call to a remote node.
		var p struct {
			NodeID string          `json:"nodeId"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		node, ok := s.topo.GetNode(p.NodeID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "node not found: " + p.NodeID}
		}
		return map[string]any{"nodeId": node.ID, "note": "remote invocation not implemented", "method": p.Method}, nil
	case "node.event":
		var p struct {
			NodeID string `json:"nodeId"`
			Event  string `json:"event"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		s.bus.Publish(GatewayEvent{Type: EventSystemEvent, Data: map[string]any{"nodeId": p.NodeID, "event": p.Event}})
		return map[string]any{"ok": true}, nil
	case "gateway.identity.get":
		return map[string]any{
			"service": "openclaw-go-gateway",
			"version": Version,
			"address": s.Address(),
			"nodes":   len(s.topo.ListNodes()),
		}, nil

	// ── device.* ─────────────────────────────────────────────────────────
	case "device.pair.init":
		var p struct {
			DeviceID string `json:"deviceId"`
			Name     string `json:"name"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		d := topology.Device{ID: p.DeviceID, Name: p.Name}
		if err := s.topo.AddDevice(d); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		req := s.topo.CreatePairing(d.ID)
		return map[string]any{"ok": true, "pairingId": req.ID, "code": req.Code, "expiresAt": req.ExpiresAt}, nil
	case "device.pair.approve":
		var p struct {
			PairingID string `json:"pairingId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if err := s.topo.ApprovePairing(p.PairingID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "pairingId": p.PairingID}, nil
	case "device.pair.reject":
		var p struct {
			PairingID string `json:"pairingId"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.PairingID) == "" {
			return nil, &rpcError{Code: -32602, Message: "pairingId is required"}
		}
		if err := s.topo.RejectPairing(p.PairingID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "rejected": p.PairingID}, nil
	case "device.pair.list", "device.pending.list":
		return map[string]any{"pending": s.topo.ListPendingPairing()}, nil
	case "device.token.list":
		devices := s.topo.ListDevices()
		return map[string]any{"devices": devices}, nil
	case "device.token.revoke":
		var p struct {
			DeviceID string `json:"deviceId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if err := s.topo.RevokeDevice(p.DeviceID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "revoked": p.DeviceID}, nil

	// ── agents.* ──────────────────────────────────────────────────────────
	case "agents.list":
		return map[string]any{"agents": s.workspace.List()}, nil
	case "agents.create":
		var profile agents.AgentProfile
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &profile); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := s.workspace.Create(profile); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": profile.ID}, nil
	case "agents.update":
		var profile agents.AgentProfile
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &profile); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := s.workspace.Update(profile); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": profile.ID}, nil
	case "agents.delete":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		deleted, err := s.workspace.Delete(p.ID)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !deleted {
			return nil, &rpcError{Code: -32001, Message: "agent not found"}
		}
		return map[string]any{"ok": true, "deleted": p.ID}, nil
	case "agents.get":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		a, ok := s.workspace.Get(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "agent not found"}
		}
		return map[string]any{"agent": a}, nil
	case "agents.files.list":
		var p struct {
			AgentID string `json:"agentId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"files": s.workspace.ListArtifacts(p.AgentID)}, nil
	case "agents.run":
		// Run a named agent profile on a message.
		var p struct {
			AgentID   string `json:"agentId"`
			SessionID string `json:"sessionId"`
			Message   string `json:"message"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if strings.TrimSpace(p.Message) == "" {
			return nil, &rpcError{Code: -32602, Message: "message is required"}
		}
		profile, ok := s.workspace.Get(p.AgentID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "agent not found: " + p.AgentID}
		}
		_ = s.store.UpsertSession(p.SessionID, "cli", "")
		var agentHistory []agents.HistoryMessage
		if sess, ok2 := s.store.Get(p.SessionID); ok2 {
			for _, m := range sess.Messages {
				agentHistory = append(agentHistory, agents.HistoryMessage{
					Role:    string(m.Role),
					Content: m.Content,
				})
			}
		}
		_ = s.store.AppendMessage(p.SessionID, sessions.Message{
			Role:      sessions.RoleUser,
			Content:   p.Message,
			CreatedAt: time.Now().UTC(),
		})
		toolFn := func(fctx context.Context, name string, args map[string]any) (any, error) {
			return s.tools.Invoke(fctx, ToolInvokeRequest{Name: name, Arguments: args})
		}
		exec := runtime.NewExecutor(s.runnerForSession(p.SessionID), toolFn)
		exec.SetSubagentFn(func(fctx context.Context, message, instructions string) (string, error) {
			var subHistory []agents.HistoryMessage
			if strings.TrimSpace(instructions) != "" {
				subHistory = []agents.HistoryMessage{{Role: "system", Content: instructions}}
			}
			reply, err := s.runnerForSession(p.SessionID).GenerateReply(fctx, agents.Turn{Message: message, History: subHistory})
			if err != nil {
				return "", err
			}
			return reply, nil
		})
		result := exec.Run(ctx, runtime.RunOptions{
			SessionID:    p.SessionID,
			Message:      p.Message,
			History:      agentHistory,
			Instructions: profile.Instructions,
			Policy: runtime.ExecPolicy{
				AllowedTools: profile.AllowedTools,
				DeniedTools:  profile.DeniedTools,
				MaxTurns:     profile.MaxTurns,
			},
			Approvals: s.approvals,
		})
		errStr := ""
		if result.Err != nil {
			errStr = result.Err.Error()
		} else if result.FinalText != "" {
			_ = s.store.AppendMessage(p.SessionID, sessions.Message{
				Role:      sessions.RoleAssistant,
				Content:   result.FinalText,
				CreatedAt: time.Now().UTC(),
			})
		}
		agentRunID := generateRunID()
		globalRunStore.put(agentRunID, result)
		return map[string]any{
			"runId":     agentRunID,
			"agentId":   p.AgentID,
			"sessionId": p.SessionID,
			"reply":     result.FinalText,
			"turns":     len(result.Turns),
			"error":     errStr,
		}, nil

	// ── artifacts.* ──────────────────────────────────────────────────────
	case "artifacts.list":
		var p struct {
			AgentID string `json:"agentId"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"artifacts": s.workspace.ListArtifacts(p.AgentID)}, nil
	case "artifacts.get":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		a, ok := s.workspace.GetArtifact(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "artifact not found"}
		}
		return map[string]any{"artifact": a}, nil
	case "artifacts.download":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		a, ok := s.workspace.GetArtifact(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "artifact not found"}
		}
		return map[string]any{"artifact": a, "content": a.Content}, nil
	case "artifacts.delete":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		deleted, err := s.workspace.DeleteArtifact(p.ID)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		if !deleted {
			return nil, &rpcError{Code: -32001, Message: "artifact not found"}
		}
		return map[string]any{"ok": true, "deleted": p.ID}, nil

	// ── talk.* (realtime voice / TTS sessions) ────────────────────────────
	case "talk.catalog":
		return map[string]any{
			"voices": []map[string]string{
				{"id": "default", "name": "Default", "language": "en-US"},
			},
			"note": "TTS provider not configured; voices are placeholders",
		}, nil
	case "talk.session.start":
		var p struct {
			SessionID string `json:"sessionId"`
			Voice     string `json:"voice"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{
			"talkSessionId": fmt.Sprintf("talk-%d", time.Now().UnixNano()),
			"sessionId":     p.SessionID,
			"status":        "started",
			"voice":         p.Voice,
		}, nil
	case "talk.session.stop":
		return map[string]any{"ok": true, "status": "stopped"}, nil
	case "talk.session.status":
		return map[string]any{"status": "idle"}, nil
	case "talk.speak":
		var p struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{
			"ok":   true,
			"text": p.Text,
			"note": "TTS not configured; text acknowledged but not spoken",
		}, nil
	case "talk.mode":
		var p struct {
			Mode string `json:"mode"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{"ok": true, "mode": p.Mode}, nil

	case "commands.list":
		return map[string]any{"commands": []string{
			"health", "gateway", "status", "sessions", "session", "message", "agent",
			"chat", "tui", "doctor", "rpc", "approvals", "approve", "reject",
			"models", "capability", "infer", "embeddings", "tools", "sandbox",
			"logs", "cron", "hooks", "secrets", "plugins", "usage", "channels",
			"nodes", "skills", "mcp", "memory", "version", "stop", "ready",
			"backup", "update", "configure", "config", "onboard",
		}}, nil

	// ── gateway.* ──────────────────────────────────────────────────────────
	case "gateway.restart", "gateway.restart.request":
		go func() {
			time.Sleep(500 * time.Millisecond)
			s.shutdownMu.Lock()
			fn := s.shutdownFn
			s.shutdownMu.Unlock()
			fn()
		}()
		return map[string]any{"ok": true, "message": "gateway shutting down for restart"}, nil
	case "gateway.restart.preflight":
		return map[string]any{"ok": true, "canRestart": true, "pendingSessions": s.store.Count()}, nil
	case "gateway.stop":
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.shutdownMu.Lock()
			fn := s.shutdownFn
			s.shutdownMu.Unlock()
			fn()
		}()
		return map[string]any{"ok": true, "message": "gateway shutting down"}, nil

	// ── wizard.* ────────────────────────────────────────────────────────
	case "wizard.start":
		return map[string]any{
			"wizardId": fmt.Sprintf("wiz-%d", time.Now().UnixNano()),
			"step":     1,
			"total":    3,
			"question": "What is your agent's name?",
		}, nil
	case "wizard.next":
		var p struct {
			WizardID string `json:"wizardId"`
			Answer   string `json:"answer"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		return map[string]any{
			"wizardId": p.WizardID,
			"step":     2,
			"total":    3,
			"question": "Which provider do you want to use? (echo/openai/anthropic)",
		}, nil
	case "wizard.cancel":
		return map[string]any{"ok": true, "cancelled": true}, nil
	case "wizard.status":
		return map[string]any{"active": false, "message": "no active wizard"}, nil

	// ── heartbeat / presence ─────────────────────────────────────────────
	case "last-heartbeat", "set-heartbeats":
		return map[string]any{"ok": true, "time": time.Now().UTC()}, nil
	case "wake", "system-presence":
		return map[string]any{"ok": true, "present": true, "time": time.Now().UTC()}, nil
	case "system-event":
		var p struct {
			Event string `json:"event"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		s.logs.Append("info", "system", "system-event: "+p.Event, nil)
		return map[string]any{"ok": true, "event": p.Event}, nil

	// ── sessions advanced ─────────────────────────────────────────────────
	case "sessions.create":
		var p struct {
			SessionID string `json:"sessionId"`
			Channel   string `json:"channel"`
			Target    string `json:"target"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Create(p.SessionID, p.Channel, p.Target); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "sessionId": p.SessionID}, nil
	case "sessions.setModel":
		var p struct {
			SessionID string `json:"sessionId"`
			Provider  string `json:"provider"`
			Model     string `json:"model"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.SetSessionModel(p.SessionID, p.Provider, p.Model); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		s.runnerCacheMu.Lock()
		delete(s.runnerCache, p.Provider+":"+p.Model)
		s.runnerCacheMu.Unlock()
		return map[string]any{"ok": true, "sessionId": p.SessionID, "provider": p.Provider, "model": p.Model}, nil
	case "sessions.preview":
		var p struct {
			SessionID string `json:"sessionId"`
			N         int    `json:"n"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		_ = json.Unmarshal(params, &p)
		if p.N <= 0 {
			p.N = 5
		}
		msgs, ok := s.store.Preview(p.SessionID, p.N)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return map[string]any{"sessionId": p.SessionID, "messages": msgs}, nil
	case "sessions.describe":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		_ = json.Unmarshal(params, &p)
		desc, ok := s.store.Describe(p.SessionID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "session not found"}
		}
		return desc, nil
	case "sessions.abort":
		var p struct {
			SessionID string `json:"sessionId"`
			Reason    string `json:"reason"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		_ = json.Unmarshal(params, &p)
		if err := s.store.Abort(p.SessionID, p.Reason); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "aborted": p.SessionID}, nil
	case "sessions.reset":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		_ = json.Unmarshal(params, &p)
		if err := s.store.Reset(p.SessionID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "reset": p.SessionID}, nil
	case "sessions.cleanup":
		var p struct {
			MaxAge string `json:"maxAge"`
		}
		if len(params) > 0 {
			json.Unmarshal(params, &p) //nolint:errcheck
		}
		maxAge := 24 * time.Hour
		if d, err := time.ParseDuration(p.MaxAge); err == nil && d > 0 {
			maxAge = d
		}
		removed, err := s.store.Cleanup(maxAge)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "removed": removed}, nil
	case "sessions.send":
		// Alias for message.send with session-centric naming.
		var req messageRequest
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if req.SessionID == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if req.Channel == "" {
			req.Channel = "cli"
		}
		reply, err := s.processMessage(ctx, req)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"sessionId": req.SessionID, "reply": reply}, nil
	case "sessions.messages.subscribe":
		var p sessionIDParams
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		evCh, unsub := s.bus.Subscribe(p.SessionID)
		defer unsub()
		deadline := time.After(100 * time.Millisecond)
		var events []GatewayEvent
		for {
			select {
			case ev := <-evCh:
				if ev.Type == EventSessionMessage {
					events = append(events, ev)
				}
			case <-deadline:
				goto sessionMsgDone
			case <-ctx.Done():
				goto sessionMsgDone
			}
		}
	sessionMsgDone:
		return map[string]any{"events": events, "sessionId": p.SessionID}, nil
	case "sessions.messages.unsubscribe":
		return map[string]any{"ok": true}, nil
	case "sessions.pluginPatch":
		// Plugin-sourced patch — same shape as sessions.patch.
		var p struct {
			SessionID string                  `json:"sessionId"`
			Patches   []sessions.MessagePatch `json:"patches"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Patch(p.SessionID, p.Patches); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "patched": len(p.Patches)}, nil
	case "sessions.unsubscribe":
		return map[string]any{"ok": true}, nil
	// ── sessions.all / sessions.count ────────────────────────────────────
	case "sessions.count":
		return map[string]any{"count": s.store.Count()}, nil

	// ── approvals.get ────────────────────────────────────────────────────
	case "approvals.get":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if req, ok := s.approvals.Get(p.ID); ok {
			return map[string]any{"approval": req}, nil
		}
		return nil, &rpcError{Code: -32001, Message: "approval not found"}

	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

// wsFrame is the envelope for all WS messages (inbound and outbound).
type wsFrame struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Channel   string          `json:"channel,omitempty"`
	Message   string          `json:"message,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Data      any             `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	Time      *time.Time      `json:"time,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(req *http.Request) bool {
			return s.isAllowedOrigin(req.Header.Get("Origin"))
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxBodyBytes) // reuse the same 4 MiB cap as HTTP bodies

	// Send a welcome/connected frame on connect.
	now := time.Now().UTC()
	_ = conn.WriteJSON(wsFrame{
		Type: "connected",
		Data: map[string]any{
			"service": "openclaw-go-gateway",
			"version": Version,
		},
		Time: &now,
	})

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	send := make(chan wsFrame, 16)
	done := make(chan struct{})

	var subCount int32 // per-connection subscription goroutine counter

	// Reader goroutine — handles framed client messages.
	go func() {
		defer close(done)
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			s.dispatchWSFrame(r.Context(), frame, send, done, &subCount)
		}
	}()

	for {
		select {
		case <-done:
			return
		case f := <-send:
			_ = conn.WriteJSON(f)
		case t := <-ticker.C:
			ts := t.UTC()
			_ = conn.WriteJSON(wsFrame{Type: "heartbeat", Time: &ts})
		}
	}
}

// maxWSSubscriptions is the maximum concurrent event subscriptions per WS connection.
const maxWSSubscriptions = 10

// dispatchWSFrame routes an inbound WS frame to the appropriate handler.
func (s *Server) dispatchWSFrame(ctx context.Context, frame wsFrame, send chan<- wsFrame, done <-chan struct{}, subCount *int32) {
	replyErr := func(id, msg string) {
		send <- wsFrame{Type: "error", ID: id, Error: msg}
	}

	switch frame.Type {
	case "ping":
		now := time.Now().UTC()
		send <- wsFrame{Type: "pong", ID: frame.ID, Time: &now}

	case "message":
		// message.send over WS — same pipeline as HTTP POST /message.
		if strings.TrimSpace(frame.SessionID) == "" {
			replyErr(frame.ID, "sessionId is required")
			return
		}
		if strings.TrimSpace(frame.Message) == "" {
			replyErr(frame.ID, "message is required")
			return
		}
		ch := frame.Channel
		if ch == "" {
			ch = "ws"
		}
		reply, err := s.processMessage(ctx, messageRequest{
			SessionID: frame.SessionID,
			Channel:   ch,
			Message:   frame.Message,
		})
		if err != nil {
			replyErr(frame.ID, err.Error())
			return
		}
		send <- wsFrame{Type: "reply", ID: frame.ID, SessionID: frame.SessionID, Message: reply}

	case "rpc":
		// JSON-RPC 2.0 over WS.
		if strings.TrimSpace(frame.Method) == "" {
			replyErr(frame.ID, "method is required")
			return
		}
		result, rpcErr := s.dispatchRPC(ctx, frame.Method, frame.Params)
		if rpcErr != nil {
			send <- wsFrame{Type: "rpc.error", ID: frame.ID, Error: rpcErr.Message}
			return
		}
		send <- wsFrame{Type: "rpc.result", ID: frame.ID, Data: result}

	case "health":
		now := time.Now().UTC()
		send <- wsFrame{Type: "health", Data: s.gatewayStatus(), Time: &now}

	case "sessions.subscribe":
		// Subscribe to events for a specific session (or all if sessionId is empty).
		// Limit concurrent subscriptions per connection to prevent goroutine exhaustion.
		if atomic.AddInt32(subCount, 1) > maxWSSubscriptions {
			atomic.AddInt32(subCount, -1)
			replyErr(frame.ID, "too many subscriptions on this connection")
			return
		}
		sessionFilter := strings.TrimSpace(frame.SessionID)
		evCh, unsub := s.bus.Subscribe(sessionFilter)
		// Forward events to the WS send channel until the connection closes
		// (done is closed) or the subscription is cancelled.
		go func() {
			defer func() {
				unsub()
				atomic.AddInt32(subCount, -1)
			}()
			for {
				select {
				case <-done:
					return
				case ev := <-evCh:
					now := time.Now().UTC()
					select {
					case send <- wsFrame{Type: "event", ID: frame.ID, Data: ev, Time: &now}:
					case <-done:
						return
					default:
					}
				}
			}
		}()
		now := time.Now().UTC()
		send <- wsFrame{Type: "subscribed", ID: frame.ID, SessionID: sessionFilter, Time: &now}

	default:
		// Unknown frame type — echo it back so clients can debug.
		send <- wsFrame{Type: "echo", ID: frame.ID, Data: frame}
	}
}

func normalizeOrigins(input []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range input {
		trimmed := strings.ToLower(strings.TrimSpace(item))
		if trimmed == "" {
			continue
		}
		out[trimmed] = struct{}{}
	}
	return out
}

// isTrustedProxy returns true if remoteIP matches any configured proxy entry,
// which may be a literal IP address or a CIDR range (e.g. "10.0.0.0/8").
func isTrustedProxy(remoteIP string, proxies []string) bool {
	ip := net.ParseIP(strings.TrimSpace(remoteIP))
	for _, proxy := range proxies {
		proxy = strings.TrimSpace(proxy)
		if proxy == "" {
			continue
		}
		if strings.Contains(proxy, "/") {
			_, network, err := net.ParseCIDR(proxy)
			if err == nil && ip != nil && network.Contains(ip) {
				return true
			}
		} else {
			// Use ParseIP for canonical comparison so IPv6 forms match
			// regardless of textual representation (e.g. ::1 vs ::0:1).
			proxyIP := net.ParseIP(proxy)
			if proxyIP != nil && ip != nil && proxyIP.Equal(ip) {
				return true
			} else if proxyIP == nil && proxy == remoteIP {
				// Unparseable entry: fall back to string equality.
				return true
			}
		}
	}
	return false
}

func (s *Server) isAllowedOrigin(origin string) bool {
	if len(s.allowedOrigins) == 0 {
		return true
	}
	trimmed := strings.ToLower(strings.TrimSpace(origin))
	if trimmed == "" {
		return false
	}
	_, ok := s.allowedOrigins[trimmed]
	return ok
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// maxBodyBytes is the maximum size accepted for JSON request bodies (4 MiB).
const maxBodyBytes = 4 << 20

// withBodyLimit wraps a handler to cap request body size at maxBodyBytes.
// json.Decoder returns an error when the limit is hit so handlers reply 400.
func withBodyLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
}

// corsAllowOrigin returns the ACAO header value for the given request origin.
// When allowedOrigins is configured it echoes the origin if it matches
// (enabling credentials), otherwise falls back to *.
func (s *Server) corsAllowOrigin(origin string) string {
	if len(s.allowedOrigins) > 0 && s.isAllowedOrigin(origin) {
		return origin
	}
	// No allowlist configured — use wildcard (safe for unauthenticated APIs).
	return "*"
}

// withCORS adds CORS headers aligned with the gateway's allowedOrigins policy.
// When an explicit allowlist is configured, it echoes the request Origin and
// sets Allow-Credentials: true so browser clients can include auth headers.
// Preflight OPTIONS requests are answered with 204 immediately.
func (s *Server) withCORSMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowOrigin := s.corsAllowOrigin(origin)
		w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-OpenClaw-Token")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if allowOrigin != "*" {
			// Allow credentials (cookies, Authorization) when using an explicit origin.
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// withCORS is a package-level alias kept for callers that don't have Server
// context; it uses the permissive wildcard policy.
func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-OpenClaw-Token")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
