package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/cronstore"
	"openclaw-go/internal/hookstore"
	"openclaw-go/internal/logstore"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/runtime"
	"openclaw-go/internal/secretstore"
	"openclaw-go/internal/sessions"

	"os"

	"github.com/gorilla/websocket"
)

type Server struct {
	host           string
	port           int
	authToken      string
	allowedOrigins map[string]struct{}
	store          *sessions.Store
	runner         agents.Runner
	route          *channels.Router
	registry       *plugins.Registry
	tools          *ToolRegistry
	approvals      *runtime.ApprovalQueue
	logs           *logstore.Store
	cron           *cronstore.Store
	hooks          *hookstore.Store
	secrets        *secretstore.Store
	rateLimiter    *RateLimiter
	bus            *EventBus
	mux            *http.ServeMux
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
	}
	if s.route == nil {
		s.route = channels.NewRouter()
	}
	s.approvals = runtime.NewApprovalQueue()
	s.rateLimiter = NewRateLimiter(120, time.Minute)
	s.bus = NewEventBus()
	s.logs, _ = logstore.New(dataDir + "/logs.json")
	s.cron, _ = cronstore.New(dataDir + "/cron.json")
	s.hooks, _ = hookstore.New(dataDir + "/hooks.json")
	s.secrets, _ = secretstore.New(dataDir + "/secrets.json")
	if s.logs == nil {
		s.logs, _ = logstore.New(os.TempDir() + "/openclaw-go-logs.json")
	}
	if s.cron == nil {
		s.cron, _ = cronstore.New(os.TempDir() + "/openclaw-go-cron.json")
	}
	if s.hooks == nil {
		s.hooks, _ = hookstore.New(os.TempDir() + "/openclaw-go-hooks.json")
	}
	if s.secrets == nil {
		s.secrets, _ = secretstore.New(os.TempDir() + "/openclaw-go-secrets.json")
	}
	s.initTools()
	s.registerRoutes()
	s.registerOpenAICompatRoutes()
	registry.MountRoutes(mux)
	return s
}

func (s *Server) Address() string {
	return fmt.Sprintf("%s:%d", s.host, s.port)
}

func (s *Server) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if pattern == "" || handler == nil {
		return
	}
	s.mux.HandleFunc(pattern, handler)
}

func (s *Server) Run(ctx context.Context) error {
	// Start cron scheduler - jobs fire on their schedule until gateway stops.
	s.cron.StartScheduler(ctx, func(cronCtx context.Context, job cronstore.Job) {
		s.logs.Append(logstore.LevelInfo, "cron", "job fired: "+job.Name, map[string]any{"id": job.ID, "schedule": job.Schedule})
		s.hooks.Emit(hookstore.EventToolInvoked, map[string]any{"cron": job.ID, "command": job.Command})
	})

	server := &http.Server{
		Addr:    s.Address(),
		Handler: s.mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.Handle("GET /tools", s.withAuth(s.handleToolsList))
	s.mux.Handle("POST /tools/invoke", s.withAuth(s.handleToolsInvoke))
	s.mux.Handle("GET /sessions", s.withAuth(s.handleListSessions))
	s.mux.Handle("GET /sessions/{id}", s.withAuth(s.handleGetSession))
	s.mux.Handle("DELETE /sessions/{id}", s.withAuth(s.handleDeleteSession))
	s.mux.Handle("GET /sessions/{id}/history", s.withAuth(s.handleSessionHistory))
	s.mux.Handle("POST /sessions/{id}/patch", s.withAuth(s.handleSessionPatch))
	s.mux.Handle("POST /sessions/{id}/kill", s.withAuth(s.handleSessionKill))
	s.mux.HandleFunc("/message", s.withAuth(s.withRateLimit(s.handleMessage)))
	s.mux.Handle("POST /agent/run", s.withAuth(s.withRateLimit(s.handleAgentRun)))
	s.mux.Handle("GET /approvals", s.withAuth(s.handleApprovalsList))
	s.mux.Handle("POST /approvals/{id}/decide", s.withAuth(s.handleApprovalDecide))
	s.mux.Handle("GET /logs", s.withAuth(s.handleLogsList))
	s.mux.Handle("GET /cron", s.withAuth(s.handleCronList))
	s.mux.Handle("POST /cron", s.withAuth(s.handleCronAdd))
	s.mux.Handle("DELETE /cron/{id}", s.withAuth(s.handleCronDelete))
	s.mux.Handle("GET /hooks", s.withAuth(s.handleHooksList))
	s.mux.Handle("POST /hooks", s.withAuth(s.handleHooksAdd))
	s.mux.Handle("DELETE /hooks/{id}", s.withAuth(s.handleHooksDelete))
	s.mux.Handle("GET /secrets", s.withAuth(s.handleSecretsList))
	s.mux.Handle("POST /secrets", s.withAuth(s.handleSecretsSet))
	s.mux.Handle("DELETE /secrets/{name}", s.withAuth(s.handleSecretsDelete))
	s.mux.HandleFunc("/rpc", s.withAuth(s.handleRPC))
	s.mux.HandleFunc("/ws", s.withAuth(s.handleWS))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "openclaw-go-gateway",
		"version": Version,
		"time":    time.Now().UTC(),
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.store.List(),
	})
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
	if strings.TrimSpace(s.authToken) == "" {
		return true
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
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
	return queryToken != "" && queryToken == s.authToken
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
	historyMessages := []agents.HistoryMessage{}
	if existing, ok := s.store.Get(req.SessionID); ok {
		for _, msg := range existing.Messages {
			historyMessages = append(historyMessages, agents.HistoryMessage{
				Role:    string(msg.Role),
				Content: msg.Content,
			})
		}
	}
	reply, err := s.runner.GenerateReply(ctx, agents.Turn{
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
	_ = s.route.Dispatch(ctx, channels.OutboundMessage{
		SessionID: req.SessionID,
		Channel:   req.Channel,
		Target:    req.Target,
		Message:   reply,
	})
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
		"authEnabled": strings.TrimSpace(s.authToken) != "",
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
		return map[string]any{"sessions": s.store.List()}, nil
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
		toolFn := func(fctx context.Context, name string, args map[string]any) (any, error) {
			return s.tools.Invoke(fctx, ToolInvokeRequest{Name: name, Arguments: args})
		}
		exec := runtime.NewExecutor(s.runner, toolFn)
		result := exec.Run(ctx, runtime.RunOptions{
			SessionID: req.SessionID,
			Message:   req.Message,
			Policy:    policy,
			Approvals: s.approvals,
		})
		var errStr string
		if result.Err != nil {
			errStr = result.Err.Error()
		}
		return map[string]any{
			"reply": result.FinalText,
			"turns": len(result.Turns),
			"error": errStr,
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
			_ = json.Unmarshal(params, &p)
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
	case "cron.add":
		return s.rpcCronAdd(params)
	case "cron.delete":
		return s.rpcCronDelete(params)
	case "hooks.list":
		return s.rpcHooksList()
	case "hooks.add":
		return s.rpcHooksAdd(params)
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

	// Reader goroutine — handles framed client messages.
	go func() {
		defer close(done)
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			s.dispatchWSFrame(r.Context(), frame, send)
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

// dispatchWSFrame routes an inbound WS frame to the appropriate handler.
func (s *Server) dispatchWSFrame(ctx context.Context, frame wsFrame, send chan<- wsFrame) {
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
		sessionFilter := strings.TrimSpace(frame.SessionID)
		evCh, unsub := s.bus.Subscribe(sessionFilter)
		go func() {
			defer unsub()
			for ev := range evCh {
				now := time.Now().UTC()
				select {
				case send <- wsFrame{Type: "event", ID: frame.ID, Data: ev, Time: &now}:
				default:
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
