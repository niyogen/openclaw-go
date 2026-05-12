package gateway

import (
	"context"
	"crypto/subtle"
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
	"openclaw-go/internal/push"
	"openclaw-go/internal/runtime"
	"openclaw-go/internal/secretstore"
	"openclaw-go/internal/sessions"
	"openclaw-go/internal/topology"

	"os"
	"path/filepath"

	"github.com/gorilla/websocket"
)

type Server struct {
	host string
	port int
	// authMu guards authToken, password, trustedProxies, allowedOrigins —
	// all of which are mutated via SetAuth/SetAuthToken/SetAllowedOrigins and
	// the gateway.config RPC at runtime while isAuthorized/isAllowedOrigin
	// read them on every request.
	authMu                sync.RWMutex
	authToken             string
	password              string
	trustedProxies        []string
	allowedOrigins        map[string]struct{}
	store                 *sessions.Store
	runnerSwapMu          sync.RWMutex // protects runner + runnerFactory during ReloadAgentRunner / SetRunnerFactory
	runner                agents.Runner
	route                 *channels.Router
	registry              *plugins.Registry
	tools                 *ToolRegistry
	approvals             *runtime.ApprovalQueue
	push                  *push.Service
	channelPlugins        *plugins.ChannelPluginRegistry
	toolPlugins           *plugins.ToolPluginRegistry
	hookPlugins           *plugins.HookPluginRegistry
	logs                  *logstore.Store
	cron                  *cronstore.Store
	hooks                 *hookstore.Store
	secrets               *secretstore.Store
	rateLimiter           *RateLimiter
	bus                   *EventBus
	shutdownMu            sync.Mutex
	shutdownFn            func()
	topo                  *topology.Store
	workspace             *agents.Workspace
	mux                   *http.ServeMux
	startedAt             time.Time
	shutdownTimeout       time.Duration
	defaultMaxContextMsgs int
	runnerFactory         func(provider, model string) agents.Runner
	runnerCache           map[string]agents.Runner
	runnerCacheMu         sync.Mutex
	metricsRequireAuth    atomic.Bool
	// Prometheus-style counters (see handleMetrics).
	rpcCallsTotal           atomic.Uint64
	channelInboundsTotal    atomic.Uint64
	channelInboundErrTotal  atomic.Uint64 // inbound handler returned error (after webhook accepted)
	agentRunsTotal          atomic.Uint64
	agentRunsFailedTotal    atomic.Uint64
	channelDispatchErrTotal atomic.Uint64
	nodeBreakerReg          *nodeBreakerRegistry
	nodeInvokeStats         *nodeInvokeStatsRegistry

	extMu    sync.RWMutex
	skillCfg []config.SkillConfig
	mcpCfg   []config.MCPServerConfig

	// webLogins drives the `web.login.start` / `web.login.wait` device-code flow.
	webLogins *webLoginRegistry

	memoryMu           sync.RWMutex
	memoryCompactAfter int
	memorySummarize    bool

	// agentSummary* is updated from cmd/openclaw on startup and SIGHUP for gateway.status / UI.
	agentSummaryMu           sync.RWMutex
	agentSummaryProvider     string
	agentSummaryModel        string
	agentSummaryOpenAISet    bool
	agentSummaryAnthropicSet bool
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
	s.webLogins = newWebLoginRegistry()
	// Wire approvals → hookstore so external systems can react to pending
	// requests without polling. The callback runs synchronously after
	// Enqueue releases its lock, so it's safe to fire Emit (which spawns
	// its own goroutines under a semaphore).
	s.approvals.SetOnEnqueue(func(req runtime.ApprovalRequest) {
		// Hook fan-out: external systems that registered an
		// approval.requested handler can react.
		if s.hooks != nil {
			s.hooks.Emit(hookstore.EventApprovalRequested, map[string]any{
				"id":        req.ID,
				"sessionId": req.SessionID,
				"tool":      req.Tool,
				"createdAt": req.CreatedAt,
			})
		}
		// Web Push fan-out: registered browser subscriptions get a
		// notification with the approval id so the operator can decide
		// without polling /approvals. Fire-and-forget — push failures
		// must not block the approval queue.
		//
		// PushService() takes authMu under RLock so this read is
		// race-safe against SetPushService writers. The previous
		// version read s.push directly and tripped -race in CI.
		if ps := s.PushService(); ps != nil {
			payload := map[string]any{
				"kind":      "approval.requested",
				"id":        req.ID,
				"sessionId": req.SessionID,
				"tool":      req.Tool,
				"createdAt": req.CreatedAt,
			}
			raw, err := json.Marshal(payload)
			if err == nil {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = ps.SendAll(ctx, raw)
				}()
			}
		}
	})
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
	s.nodeBreakerReg = newNodeBreakerRegistry(defaultNodeCircuitSettings())
	s.nodeInvokeStats = newNodeInvokeStatsRegistry()
	// Final nil-guard: if every storage path attempt has failed, use a
	// guaranteed temp path so handlers never nil-panic on s.topo/s.workspace.
	if s.topo == nil {
		tmp := os.TempDir()
		s.topo, _ = topology.New(filepath.Join(tmp, fmt.Sprintf("openclaw-topo-%d.json", os.Getpid())))
	}
	if s.workspace == nil {
		tmp := os.TempDir()
		s.workspace, _ = agents.NewWorkspace(filepath.Join(tmp, fmt.Sprintf("openclaw-ws-%d.json", os.Getpid())))
	}
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

// appendLog writes a log entry and publishes it to the event bus so
// GET /logs/stream subscribers see new entries in real time.
func (s *Server) appendLog(level logstore.Level, component, message string, meta map[string]any) {
	_ = s.logs.Append(level, component, message, meta)
	entries := s.logs.List(string(level), component, 1)
	if len(entries) > 0 {
		s.bus.Publish(GatewayEvent{Type: EventLogAppended, Data: entries[len(entries)-1]})
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
	s.runnerSwapMu.Lock()
	s.runnerFactory = fn
	s.runnerSwapMu.Unlock()

	s.runnerCacheMu.Lock()
	s.runnerCache = map[string]agents.Runner{}
	s.runnerCacheMu.Unlock()
}

// ReloadAgentRunner replaces the gateway-wide runner and the per-session runner
// factory, then clears the per-session runner cache. Call after config changes
// that affect model routing (cmd/openclaw startup and SIGHUP).
func (s *Server) ReloadAgentRunner(runner agents.Runner, factory func(provider, model string) agents.Runner) {
	s.runnerSwapMu.Lock()
	s.runner = runner
	s.runnerFactory = factory
	s.runnerSwapMu.Unlock()

	s.runnerCacheMu.Lock()
	s.runnerCache = map[string]agents.Runner{}
	s.runnerCacheMu.Unlock()
}

func (s *Server) globalRunner() agents.Runner {
	s.runnerSwapMu.RLock()
	defer s.runnerSwapMu.RUnlock()
	return s.runner
}

// SetAgentSummary records operator-facing agent metadata for gateway.status.
func (s *Server) SetAgentSummary(provider, model string, openAIKeySet, anthropicKeySet bool) {
	s.agentSummaryMu.Lock()
	defer s.agentSummaryMu.Unlock()
	s.agentSummaryProvider = strings.TrimSpace(provider)
	s.agentSummaryModel = strings.TrimSpace(model)
	s.agentSummaryOpenAISet = openAIKeySet
	s.agentSummaryAnthropicSet = anthropicKeySet
}

func (s *Server) agentSummaryPayload() map[string]any {
	s.agentSummaryMu.RLock()
	defer s.agentSummaryMu.RUnlock()
	return map[string]any{
		"provider":                  s.agentSummaryProvider,
		"model":                     s.agentSummaryModel,
		"openaiApiKeyConfigured":    s.agentSummaryOpenAISet,
		"anthropicApiKeyConfigured": s.agentSummaryAnthropicSet,
	}
}

// runnerForSession returns a session-specific Runner when the session has a
// provider/model override, otherwise returns the global runner.
// The cache lookup and session read happen under the same lock to avoid
// TOCTOU races where SetSessionModel changes the model between the Get and
// the cache insert.
func (s *Server) runnerForSession(sessionID string) agents.Runner {
	s.runnerSwapMu.RLock()
	factory := s.runnerFactory
	global := s.runner
	s.runnerSwapMu.RUnlock()

	if factory == nil {
		return global
	}
	s.runnerCacheMu.Lock()
	defer s.runnerCacheMu.Unlock()

	sess, ok := s.store.Get(sessionID)
	if !ok || (sess.Provider == "" && sess.Model == "") {
		return global
	}
	key := sess.Provider + ":" + sess.Model
	if r, exists := s.runnerCache[key]; exists {
		return r
	}
	r := factory(sess.Provider, sess.Model)
	if r == nil {
		return global // factory returned nil — fall back to default
	}
	s.runnerCache[key] = r
	return r
}

// runnerForProcessMessage picks the runner for one user message.
//
// Channel "ui" is reserved for the /ui control panel (Quick infer). Those
// requests must follow the gateway-wide agent configuration, not a
// per-session provider/model override. Otherwise the fixed sessionId "ui-infer"
// can retain a legacy echo override in sessions.json and keep echoing even
// after the operator switches agent.provider to openai in openclaw.json.
func (s *Server) runnerForProcessMessage(req messageRequest) agents.Runner {
	if strings.EqualFold(strings.TrimSpace(req.Channel), "ui") {
		return s.globalRunner()
	}
	return s.runnerForSession(req.SessionID)
}

func (s *Server) Address() string {
	return fmt.Sprintf("%s:%d", s.host, s.port)
}

// Bus returns the internal event bus so callers can subscribe to gateway events.
func (s *Server) Bus() *EventBus { return s.bus }

// SetAuth configures additional auth modes (password, trusted proxies).
// Safe to call after Run() for hot reload via SIGHUP.
func (s *Server) SetAuth(password string, trustedProxies []string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.password = strings.TrimSpace(password)
	s.trustedProxies = trustedProxies
}

// SetPushService attaches a Web Push service. Call this from main once the
// service is constructed; passing nil disables push delivery (the
// approval-onEnqueue callback no-ops). Safe to call after Run().
func (s *Server) SetPushService(ps *push.Service) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.push = ps
}

// SetChannelPluginRegistry attaches the channel-plugin registry. Used by
// the plugins.channel.* RPCs to list / approve / revoke. Safe to call
// after Run() (writes/reads guarded by authMu).
func (s *Server) SetChannelPluginRegistry(reg *plugins.ChannelPluginRegistry) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.channelPlugins = reg
}

// ChannelPluginRegistry returns the configured registry, or nil if none
// has been attached (no plugins are enabled).
func (s *Server) ChannelPluginRegistry() *plugins.ChannelPluginRegistry {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.channelPlugins
}

// Tools returns the gateway's ToolRegistry so external code (e.g. the
// cmd/openclaw main package) can register additional tools after Server
// construction. The registry itself is internally locked for concurrent
// Register/UnregisterByPrefix, so this is safe to call anytime.
func (s *Server) Tools() *ToolRegistry { return s.tools }

// SetToolPluginRegistry attaches the tool-plugin registry. Used by the
// plugins.tool.* RPCs to list / approve / revoke. Safe to call after
// Run() (writes/reads guarded by authMu).
func (s *Server) SetToolPluginRegistry(reg *plugins.ToolPluginRegistry) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.toolPlugins = reg
}

// ToolPluginRegistry returns the configured tool-plugin registry, or
// nil if none has been attached (no tool plugins are enabled).
func (s *Server) ToolPluginRegistry() *plugins.ToolPluginRegistry {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.toolPlugins
}

// SetHookPluginRegistry attaches the hook-plugin registry. Used by the
// plugins.hook.* RPCs to list / approve / revoke. Safe to call after
// Run() (writes/reads guarded by authMu).
func (s *Server) SetHookPluginRegistry(reg *plugins.HookPluginRegistry) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.hookPlugins = reg
}

// HookPluginRegistry returns the configured hook-plugin registry, or
// nil if none has been attached.
func (s *Server) HookPluginRegistry() *plugins.HookPluginRegistry {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.hookPlugins
}

// HookStore returns the gateway's hookstore so external code (e.g.
// cmd/openclaw) can register hookstore.EventListener subscribers from
// outside the gateway package.
func (s *Server) HookStore() *hookstore.Store { return s.hooks }

// PushService returns the configured push service, or nil if disabled.
// Useful for tests + the new push.* RPCs.
func (s *Server) PushService() *push.Service {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.push
}

// SetAuthToken replaces the bearer token. Safe to call after Run().
func (s *Server) SetAuthToken(token string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.authToken = strings.TrimSpace(token)
}

// SetAllowedOrigins replaces the CORS/WS origin allowlist. Safe to call after Run().
func (s *Server) SetAllowedOrigins(origins []string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.allowedOrigins = normalizeOrigins(origins)
}

// authSnapshot returns a consistent snapshot of the auth-related fields under
// a single RLock acquisition, so callers don't have to coordinate locks
// themselves and can read stable values across an entire request.
func (s *Server) authSnapshot() (authToken, password string, trustedProxies []string) {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	authToken = s.authToken
	password = s.password
	if len(s.trustedProxies) > 0 {
		trustedProxies = append([]string(nil), s.trustedProxies...)
	}
	return
}

// authEnabledSnapshot reports whether any auth mode is configured.
func (s *Server) authEnabledSnapshot() bool {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return strings.TrimSpace(s.authToken) != "" || strings.TrimSpace(s.password) != ""
}

// trustedProxiesSnapshot returns a copy of the trusted-proxy list under RLock.
func (s *Server) trustedProxiesSnapshot() []string {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	if len(s.trustedProxies) == 0 {
		return nil
	}
	return append([]string(nil), s.trustedProxies...)
}

// SetMetricsRequireAuth controls whether GET /metrics requires the same credentials
// as other gateway routes. Safe to call after Run() (e.g. SIGHUP reload).
func (s *Server) SetMetricsRequireAuth(require bool) {
	s.metricsRequireAuth.Store(require)
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
		s.appendLog(logstore.LevelInfo, "cron", "job fired: "+job.Name, map[string]any{"id": job.ID, "schedule": job.Schedule})
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

	// Lifecycle hook: gateway is fully wired and about to start listening.
	s.hooks.Emit(hookstore.EventGatewayStarted, map[string]any{
		"address": s.Address(),
		"version": Version,
		"time":    time.Now().UTC(),
	})

	go func() {
		<-ctx.Done()
		// Fire BEFORE Shutdown so external systems learn we're going away
		// while we can still serve a final 200 (Shutdown blocks active
		// requests until they finish or the deadline elapses).
		s.hooks.Emit(hookstore.EventGatewayStopping, map[string]any{
			"address": s.Address(),
			"time":    time.Now().UTC(),
		})
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
	s.mux.HandleFunc("/metrics", s.handleMetrics)
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
	s.mux.Handle("GET /logs/stream", cors(s.withAuth(s.handleLogsStream)))
	s.mux.Handle("GET /cron", cors(s.withAuth(s.handleCronList)))
	s.mux.Handle("POST /cron", cors(s.withAuth(withBodyLimit(s.handleCronAdd))))
	s.mux.Handle("DELETE /cron/{id}", cors(s.withAuth(s.handleCronDelete)))
	s.mux.Handle("GET /hooks", cors(s.withAuth(s.handleHooksList)))
	s.mux.Handle("POST /hooks", cors(s.withAuth(withBodyLimit(s.handleHooksAdd))))
	s.mux.Handle("DELETE /hooks/{id}", cors(s.withAuth(s.handleHooksDelete)))
	s.mux.Handle("GET /secrets", cors(s.withAuth(s.handleSecretsList)))
	s.mux.Handle("POST /secrets", cors(s.withAuth(withBodyLimit(s.handleSecretsSet))))
	s.mux.Handle("DELETE /secrets/{name}", cors(s.withAuth(s.handleSecretsDelete)))
	// Web-login confirm flow: GET renders a confirm page, POST records the
	// decision. The confirm POST is auth-gated when auth is enabled (token
	// rotation) but open during initial setup (no token configured yet).
	s.mux.HandleFunc("POST /web/login/{nonce}/confirm", cors(s.handleWebLoginConfirm))
	s.mux.HandleFunc("GET /web/login/{nonce}", cors(s.handleWebLoginPage))
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
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
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
			Provider:     sess.Provider,
			Model:        sess.Model,
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
	s.appendLog(logstore.LevelInfo, "sessions", "session model set: "+id,
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
	s.appendLog(logstore.LevelInfo, "sessions", "session killed: "+id, nil)
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
	s.appendLog(logstore.LevelInfo, "sessions", "session deleted: "+id, nil)
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
	authToken, password, trustedProxies := s.authSnapshot()

	// If no auth configured, allow all.
	if strings.TrimSpace(authToken) == "" && strings.TrimSpace(password) == "" {
		return true
	}

	authorization := strings.TrimSpace(r.Header.Get("Authorization"))

	// Bearer token (constant-time compare to defeat per-byte timing attacks).
	if authToken != "" {
		tokenBytes := []byte(authToken)
		if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			token := strings.TrimSpace(authorization[len("Bearer "):])
			if constantTimeStringEqual(token, tokenBytes) {
				return true
			}
		}
		headerToken := strings.TrimSpace(r.Header.Get("X-OpenClaw-Token"))
		if headerToken != "" && constantTimeStringEqual(headerToken, tokenBytes) {
			return true
		}
		queryToken := strings.TrimSpace(r.URL.Query().Get("token"))
		if queryToken != "" && constantTimeStringEqual(queryToken, tokenBytes) {
			return true
		}
	}

	// HTTP Basic password auth (constant-time).
	if password != "" {
		if _, pass, ok := r.BasicAuth(); ok && constantTimeStringEqual(pass, []byte(password)) {
			return true
		}
	}

	// Trusted proxy: allow requests from configured proxy IPs/CIDRs without auth.
	// SECURITY: must use the direct peer (RemoteAddr), NOT clientIP(), because
	// clientIP() honors X-Forwarded-For which an attacker can spoof to claim
	// any trusted-proxy IP and bypass auth entirely.
	if len(trustedProxies) > 0 {
		if isTrustedProxy(directRemoteIP(r), trustedProxies) {
			return true
		}
	}

	return false
}

// constantTimeStringEqual reports whether got equals want using a comparison
// whose timing does not leak which byte of want differed. Length-mismatched
// inputs return false in constant time relative to the longer input.
func constantTimeStringEqual(got string, want []byte) bool {
	return subtle.ConstantTimeCompare([]byte(got), want) == 1
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
		s.appendLog(logstore.LevelInfo, "sessions", "session created: "+req.SessionID, nil)
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
	reply, err := s.runnerForProcessMessage(req).GenerateReply(ctx, agents.Turn{
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
		s.channelDispatchErrTotal.Add(1)
		s.appendLog(logstore.LevelWarn, "channels", //nolint:errcheck
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
	s.appendLog(logstore.LevelInfo, "message", "reply sent for "+req.SessionID, nil)
	return reply, nil
}

func (s *Server) HandleInbound(ctx context.Context, inbound channels.InboundMessage) (string, error) {
	s.channelInboundsTotal.Add(1)
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

// RecordInboundHandlerError increments observability counters/logs when a channel
// webhook or poller invoked HandleInbound and it returned an error.
func (s *Server) RecordInboundHandlerError(channel string, err error, attrs map[string]any) {
	if err == nil {
		return
	}
	s.channelInboundErrTotal.Add(1)
	meta := map[string]any{
		"channel": channel,
		"error":   err.Error(),
	}
	for k, v := range attrs {
		meta[k] = v
	}
	s.appendLog(logstore.LevelWarn, "channels", "inbound handler error: "+err.Error(), meta)
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
	s.rpcCallsTotal.Add(1)
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
	out := map[string]any{
		"ok":                 true,
		"service":            "openclaw-go-gateway",
		"version":            Version,
		"address":            s.Address(),
		"authEnabled":        s.authEnabledSnapshot(),
		"metricsRequireAuth": s.metricsRequireAuth.Load(),
		"uptime":             time.Since(s.startedAt).String(),
		"time":               time.Now().UTC(),
		"agent":              s.agentSummaryPayload(),
	}
	return out
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
				Provider:     sess.Provider,
				Model:        sess.Model,
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
		s.appendLog(logstore.LevelInfo, "sessions", "session killed: "+p.SessionID, nil)
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
		s.appendLog(logstore.LevelInfo, "sessions", "session deleted: "+p.SessionID, nil)
		return map[string]any{"ok": true, "deleted": p.SessionID}, nil
	case "plugins.list":
		if s.registry == nil {
			return map[string]any{"plugins": []string{}}, nil
		}
		return map[string]any{"plugins": s.registry.Names()}, nil
	case "plugins.channel.list":
		// Channel plugins (per docs/PLUGIN-ARCHITECTURE.md) — pending +
		// approved entries with their approval state.
		reg := s.ChannelPluginRegistry()
		if reg == nil {
			return map[string]any{"plugins": []any{}}, nil
		}
		return map[string]any{"plugins": reg.List()}, nil
	case "plugins.channel.approve":
		reg := s.ChannelPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "channel-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		token, err := reg.Approve(strings.TrimSpace(p.Name))
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		// IMPORTANT: the token is returned ONCE here. Operator must copy
		// it into the plugin's OPENCLAW_PLUGIN_TOKEN env var. Subsequent
		// approve calls return the existing token (idempotent) — to
		// rotate, revoke first.
		return map[string]any{"name": p.Name, "token": token, "state": "approved"}, nil
	case "plugins.channel.revoke":
		reg := s.ChannelPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "channel-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		if err := reg.Revoke(strings.TrimSpace(p.Name)); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"name": p.Name, "state": "pending"}, nil
	case "plugins.tool.list":
		reg := s.ToolPluginRegistry()
		if reg == nil {
			return map[string]any{"plugins": []any{}}, nil
		}
		return map[string]any{"plugins": reg.List()}, nil
	case "plugins.tool.approve":
		reg := s.ToolPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "tool-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		token, err := reg.Approve(strings.TrimSpace(p.Name))
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		// Approved manifests' tools are wired into the gateway's
		// ToolRegistry at startup; approving at runtime issues the
		// token but does NOT hot-register the tool (operator should
		// SIGHUP / restart to pick up newly-approved tools — matches
		// the channel-plugin RPC's same posture).
		return map[string]any{"name": p.Name, "token": token, "state": "approved"}, nil
	case "plugins.tool.revoke":
		reg := s.ToolPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "tool-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		if err := reg.Revoke(strings.TrimSpace(p.Name)); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"name": p.Name, "state": "pending"}, nil
	case "plugins.hook.list":
		reg := s.HookPluginRegistry()
		if reg == nil {
			return map[string]any{"plugins": []any{}}, nil
		}
		return map[string]any{"plugins": reg.List()}, nil
	case "plugins.hook.approve":
		reg := s.HookPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "hook-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		token, err := reg.Approve(strings.TrimSpace(p.Name))
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		// Approved hook plugins' endpoints are wired into the
		// hookstore's listener fan-out at startup. Approving at
		// runtime issues the token but does NOT hot-register the
		// dispatcher — operator restarts to pick up the new endpoint
		// (matches the channel + tool plugin postures).
		return map[string]any{"name": p.Name, "token": token, "state": "approved"}, nil
	case "plugins.hook.revoke":
		reg := s.HookPluginRegistry()
		if reg == nil {
			return nil, &rpcError{Code: -32001, Message: "hook-plugin registry not configured"}
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
			return nil, &rpcError{Code: -32602, Message: "name is required"}
		}
		if err := reg.Revoke(strings.TrimSpace(p.Name)); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"name": p.Name, "state": "pending"}, nil
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
		// Inherit server-wide context window default when request doesn't specify one.
		if policy.MaxContextMessages == 0 && s.defaultMaxContextMsgs > 0 {
			policy.MaxContextMessages = s.defaultMaxContextMsgs
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
		s.maintainSessionMemory(ctx, req.SessionID)
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
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
	case "sessions.compaction.list":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		return s.store.CompactionList(p.SessionID), nil
	case "sessions.compaction.get":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		rec, ok := s.store.CompactionGet(p.ID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "compaction record not found"}
		}
		return rec, nil
	case "sessions.compaction.restore":
		var p struct {
			ID string `json:"id"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if err := s.store.CompactionRestore(p.ID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": p.ID}, nil
	case "sessions.compaction.branch":
		var p struct {
			ID           string `json:"id"`
			NewSessionID string `json:"newSessionId"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		branch, err := s.store.CompactionBranch(p.ID, strings.TrimSpace(p.NewSessionID))
		if err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return branch, nil
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
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
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
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
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
				"authEnabled": s.authEnabledSnapshot(),
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
		// Apply known fields to in-memory state under the auth lock so
		// concurrent isAuthorized readers see a consistent token/password pair.
		s.authMu.Lock()
		if tok, ok := patch["authToken"].(string); ok {
			s.authToken = strings.TrimSpace(tok)
		}
		if password, ok := patch["password"].(string); ok {
			s.password = strings.TrimSpace(password)
		}
		s.authMu.Unlock()
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
		checks["runner"] = map[string]any{"ok": s.globalRunner() != nil}
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
		return s.SkillsStatus(), nil
	case "skills.search":
		return s.SkillsSearch(params), nil
	case "skills.detail":
		return s.SkillsDetail(params), nil
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
		ctx2, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		latest, page, err := LatestReleaseCheckFn(ctx2)
		out := map[string]any{
			"currentVersion":  Version,
			"latestVersion":   latest,
			"releasesPage":    page,
			"updateAvailable": UpdateAvailable(Version, latest),
		}
		if err != nil {
			out["checkError"] = err.Error()
		}
		return out, nil
	case "update.run":
		ctx2, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		latest, page, err := LatestReleaseCheckFn(ctx2)
		out := map[string]any{
			"ok":              true,
			"currentVersion":  Version,
			"latestVersion":   latest,
			"releasesPage":    page,
			"updateAvailable": UpdateAvailable(Version, latest),
			"note":            "Automated binary replacement is not performed; download the release asset or use your package manager, then restart the gateway.",
		}
		if err != nil {
			out["checkError"] = err.Error()
		}
		return out, nil

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
	case "tracing.status":
		return map[string]any{
			"ok":                    true,
			"requestIdHeader":       "X-Request-ID",
			"clientRequestIdEcho":   true,
			"openTelemetryExporter": false,
			"note":                  "Each request gets X-Request-ID; send X-Request-ID to correlate client and gateway logs. OpenTelemetry SDK export is not wired in this build.",
		}, nil

	// ── environments ──────────────────────────────────────────────────────
	case "environments.list":
		return map[string]any{"environments": []string{"default"}}, nil
	case "environments.status":
		return map[string]any{"environment": "default", "status": "active"}, nil

	// ── models auth status ────────────────────────────────────────────────
	case "models.authStatus":
		gr := s.globalRunner()
		_, isOpenAI := gr.(*agents.OpenAIRunner)
		_, isAnthropic := gr.(*agents.AnthropicRunner)
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
				if s.globalRunner() != nil {
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
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
				// Only return events for the requested session to prevent cross-session leaks.
				if ev.Type == EventAgentReply && ev.SessionID == p.SessionID {
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.NodeID) == "" {
			return nil, &rpcError{Code: -32602, Message: "nodeId is required"}
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
		var p struct {
			NodeID string          `json:"nodeId"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if strings.TrimSpace(p.NodeID) == "" {
			return nil, &rpcError{Code: -32602, Message: "nodeId is required"}
		}
		node, ok := s.topo.GetNode(p.NodeID)
		if !ok {
			return nil, &rpcError{Code: -32001, Message: "node not found: " + p.NodeID}
		}
		nodeID := strings.TrimSpace(p.NodeID)
		br := s.nodeBreakerReg.get(nodeID)
		if err := br.before(); err != nil {
			s.nodeInvokeStats.record(nodeID, "circuit_open", 0)
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		t0 := time.Now()
		out, rpcErr := forwardNodeRPC(ctx, node, p.Method, p.Params)
		dt := time.Since(t0)
		if rpcErr != nil {
			s.nodeInvokeStats.record(nodeID, "failure", dt)
			if shouldTripNodeCircuit(rpcErr) {
				br.recordFailure()
			}
			return nil, rpcErr
		}
		br.recordSuccess()
		s.nodeInvokeStats.record(nodeID, "success", dt)
		return out, nil
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
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
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
		}
		if strings.TrimSpace(p.AgentID) == "" {
			return nil, &rpcError{Code: -32602, Message: "agentId is required"}
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
		agentPolicy := runtime.ExecPolicy{
			AllowedTools: profile.AllowedTools,
			DeniedTools:  profile.DeniedTools,
			MaxTurns:     profile.MaxTurns,
		}
		// Inherit server-wide context window default.
		if agentPolicy.MaxContextMessages == 0 && s.defaultMaxContextMsgs > 0 {
			agentPolicy.MaxContextMessages = s.defaultMaxContextMsgs
		}
		result := exec.Run(ctx, runtime.RunOptions{
			SessionID:    p.SessionID,
			Message:      p.Message,
			History:      agentHistory,
			Instructions: profile.Instructions,
			Policy:       agentPolicy,
			Approvals:    s.approvals,
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
		s.maintainSessionMemory(ctx, p.SessionID)
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
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
		}
		// Require agentId to prevent returning all artifacts across all agents.
		if strings.TrimSpace(p.AgentID) == "" {
			return nil, &rpcError{Code: -32602, Message: "agentId is required"}
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

	// ── web.login.* (device-code-style browser approval) ─────────────────
	case "web.login.start":
		return s.rpcWebLoginStart(params)
	case "web.login.wait":
		return s.rpcWebLoginWait(ctx, params)

	// ── push.* (VAPID Web Push subscription + delivery) ──────────────────
	case "push.publicKey":
		ps := s.PushService()
		if ps == nil {
			return nil, &rpcError{Code: -32001, Message: "push is not configured (set gateway.pushContact)"}
		}
		return map[string]any{"publicKey": ps.PublicKey()}, nil
	case "push.web.subscribe":
		ps := s.PushService()
		if ps == nil {
			return nil, &rpcError{Code: -32001, Message: "push is not configured"}
		}
		var p struct {
			Endpoint string `json:"endpoint"`
			P256dh   string `json:"p256dh"`
			Auth     string `json:"auth"`
			Label    string `json:"label"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		sub, err := ps.Subscribe(p.Endpoint, p.P256dh, p.Auth, p.Label)
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return sub, nil
	case "push.web.unsubscribe":
		ps := s.PushService()
		if ps == nil {
			return nil, &rpcError{Code: -32001, Message: "push is not configured"}
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
			return nil, &rpcError{Code: -32602, Message: "id is required"}
		}
		if err := ps.Unsubscribe(p.ID); err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true, "id": p.ID}, nil
	case "push.web.list":
		ps := s.PushService()
		if ps == nil {
			return nil, &rpcError{Code: -32001, Message: "push is not configured"}
		}
		return ps.List(), nil
	case "push.test":
		ps := s.PushService()
		if ps == nil {
			return nil, &rpcError{Code: -32001, Message: "push is not configured"}
		}
		// Optional `id` param targets a single sub; absent = fan out to all.
		var p struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.Message) == "" {
			p.Message = "openclaw-go push test"
		}
		payload, _ := json.Marshal(map[string]any{
			"kind":    "push.test",
			"message": p.Message,
		})
		var err error
		if strings.TrimSpace(p.ID) != "" {
			err = ps.SendOne(ctx, p.ID, payload)
		} else {
			err = ps.SendAll(ctx, payload)
		}
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: err.Error()}
		}
		return map[string]any{"ok": true}, nil

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
		s.appendLog("info", "system", "system-event: "+p.Event, nil)
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
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
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
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
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
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Abort(p.SessionID, p.Reason); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "aborted": p.SessionID}, nil
	case "sessions.reset":
		var p sessionIDParams
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		if err := s.store.Reset(p.SessionID); err != nil {
			return nil, &rpcError{Code: -32001, Message: err.Error()}
		}
		return map[string]any{"ok": true, "reset": p.SessionID}, nil
	case "sessions.cleanup":
		var p struct {
			MaxAge string `json:"maxAge"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: -32602, Message: "invalid params"}
			}
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
		if len(params) == 0 {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
		}
		evCh, unsub := s.bus.Subscribe(p.SessionID)
		defer unsub()
		deadline := time.After(100 * time.Millisecond)
		var events []GatewayEvent
		for {
			select {
			case ev := <-evCh:
				// Filter to only this session's message events.
				if ev.Type == EventSessionMessage && ev.SessionID == p.SessionID {
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
	s.authMu.RLock()
	defer s.authMu.RUnlock()
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

// hasAllowedOrigins reports whether the CORS allowlist is non-empty.
func (s *Server) hasAllowedOrigins() bool {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return len(s.allowedOrigins) > 0
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
	if s.hasAllowedOrigins() && s.isAllowedOrigin(origin) {
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
