package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// /control/ws is the upstream-openclaw-compatible WebSocket endpoint.
//
// It exists so frontends written against the upstream openclaw protocol
// (openclaw-studio, openclaw-nerve, native apps, …) can connect to
// openclaw-go without modification. The on-the-wire framing matches
// upstream openclaw — distinct from our native /ws which uses a simpler
// in-house shape.
//
// Phase 1 (this file): connect handshake only.
//   - On upgrade: server emits `{type:"event", event:"connect.challenge"}`.
//   - Client replies `{type:"req", id, method:"connect", params:{...auth.token,...}}`.
//   - Server validates token + protocol range, replies `{type:"res", id, ok:true|false}`.
//   - After successful connect, server treats subsequent frames as method
//     requests. Phase 1 returns METHOD_NOT_FOUND for everything except `wake`
//     (which is a no-op heartbeat — used by clients to probe liveness).
//
// Phase 2+ will register concrete method handlers in dispatchControlMethod.

// controlProtocolVersion is the only WS protocol version openclaw-go
// currently speaks. Studio sends minProtocol=maxProtocol=3 so this
// matches; older / newer clients are rejected at connect time.
const controlProtocolVersion = 3

// controlConnectTimeout caps how long an upgraded socket may stay
// connected without completing the connect handshake. Studio's client
// timeout is 8s; we give ourselves 5s headroom.
const controlConnectTimeout = 5 * time.Second

// controlMaxConcurrentRequests bounds in-flight goroutines per single
// WebSocket connection. Studio's bootstrap fan-out fires ~6 requests in
// parallel; 32 leaves ample headroom while preventing a malicious
// client from spawning unbounded goroutines.
const controlMaxConcurrentRequests = 32

// controlScopes lists every operator scope the connect handler grants.
// We currently grant all 5 unconditionally because openclaw-go does not
// yet implement scope-enforced authz (tracked as a separate parity
// item). Real scope enforcement is a follow-up.
var controlScopes = []string{
	"operator.admin",
	"operator.read",
	"operator.write",
	"operator.approvals",
	"operator.pairing",
}

// controlFrame mirrors the upstream openclaw WS frame shape exactly.
// Studio's adapter parses by .type:
//   - "event": server → client push. event name in .event, body in .payload.
//   - "req":   client → server method call. id, method, params.
//   - "res":   server → client method response. id, ok, payload OR error.
type controlFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Payload any             `json:"payload,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Error   *controlError   `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
	Seq     *int64          `json:"seq,omitempty"`
}

// controlError is the upstream error envelope studio's adapter reads
// (frame.error?.code/message/details).
type controlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// connectParams decodes the params field of a `req method=connect`
// frame. Only the fields we currently validate are decoded; extras are
// ignored so additions in upstream don't break our parser.
type connectParams struct {
	MinProtocol int      `json:"minProtocol"`
	MaxProtocol int      `json:"maxProtocol"`
	Role        string   `json:"role"`
	Scopes      []string `json:"scopes"`
	Caps        []string `json:"caps"`
	Auth        struct {
		Token string `json:"token"`
	} `json:"auth"`
	Client struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		Platform string `json:"platform"`
		Mode     string `json:"mode"`
	} `json:"client"`
}

// handleControlWS upgrades the HTTP connection, sends the connect
// challenge, then drives the reader/writer goroutines until the client
// disconnects or the handshake times out.
//
// Unlike s.handleWS, /control/ws is NOT wrapped in s.withAuth — the
// token lives inside the connect message body per the upstream
// protocol. To prevent an unauthed client from holding the socket open,
// we enforce controlConnectTimeout: if no successful connect arrives
// inside that window, the connection is closed.
func (s *Server) handleControlWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(req *http.Request) bool {
			// Node WS clients (studio's server-owned adapter) don't
			// send Origin headers, so empty Origin is allowed. Browser
			// origins still go through the same allowlist as /ws.
			origin := req.Header.Get("Origin")
			if origin == "" {
				return true
			}
			return s.isAllowedOrigin(origin)
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxBodyBytes)

	// Mutex-guarded writer: every goroutine that sends a frame must go
	// through writeFrame. Gorilla WebSocket forbids concurrent writes
	// on a single connection.
	var writeMu sync.Mutex
	writeFrame := func(f controlFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(f)
	}

	// Send the handshake challenge immediately on upgrade.
	if err := writeFrame(controlFrame{Type: "event", Event: "connect.challenge"}); err != nil {
		return
	}

	// connected gates whether incoming frames are treated as method
	// requests. Until the connect handshake succeeds, only `connect`
	// is accepted; everything else is dropped.
	var connected atomic.Bool

	// connectDone closes when the connect handshake succeeds. The
	// handshake-timeout goroutine watches it as a cancel signal so
	// there is no race window between Load() and Store() — a successful
	// connect deterministically prevents the timeout from closing
	// the socket. Guarded by closeOnce so handleControlConnect can
	// signal idempotently.
	connectDone := make(chan struct{})
	var connectOnce sync.Once
	signalConnected := func() {
		connectOnce.Do(func() {
			connected.Store(true)
			close(connectDone)
		})
	}

	// Handshake timeout: if connect doesn't succeed inside the window
	// the connection is closed so an unauthed client can't camp on the
	// socket. Goroutine exits cleanly on success, request cancel, or
	// timeout.
	go func() {
		select {
		case <-time.After(controlConnectTimeout):
			_ = conn.Close()
		case <-connectDone:
			return
		case <-r.Context().Done():
			return
		}
	}()

	// Per-connection semaphore for concurrent in-flight method
	// handlers. Bounded so a slow/buggy plugin can't OOM the gateway
	// by firing 10k requests on one socket.
	sem := make(chan struct{}, controlMaxConcurrentRequests)

	for {
		var frame controlFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		s.handleControlFrame(r.Context(), frame, &connected, signalConnected, writeFrame, sem)
	}
}

// handleControlFrame routes one inbound frame. The connect path is
// inline (must complete before other methods can run). Post-connect
// method requests are dispatched in goroutines, throttled by sem.
func (s *Server) handleControlFrame(
	ctx context.Context,
	frame controlFrame,
	connected *atomic.Bool,
	signalConnected func(),
	writeFrame func(controlFrame) error,
	sem chan struct{},
) {
	if frame.Type != "req" {
		// Non-request frames (event echoes, garbage, …) are silently
		// ignored. The upstream protocol is server→client for events;
		// a client should never originate `event` frames.
		return
	}

	// Request frames must carry a non-empty id — the upstream protocol
	// is response-by-id and an empty id can never pair with a pending
	// client request. Reject upfront so a misbehaving client gets a
	// loud error instead of silently hanging on a response that never
	// arrives.
	if strings.TrimSpace(frame.ID) == "" {
		_ = writeFrame(controlErrorResponse("", "INVALID_REQUEST", "request id is required", nil))
		return
	}

	method := strings.TrimSpace(frame.Method)
	if method == "" {
		_ = writeFrame(controlErrorResponse(frame.ID, "INVALID_REQUEST", "method is required", nil))
		return
	}

	// `connect` is special-cased: must run inline (state mutation),
	// and is only valid pre-connect. Re-sending connect on an already-
	// connected socket is rejected.
	if method == "connect" {
		if connected.Load() {
			_ = writeFrame(controlErrorResponse(frame.ID, "INVALID_REQUEST", "already connected", nil))
			return
		}
		s.handleControlConnect(frame, signalConnected, writeFrame)
		return
	}

	if !connected.Load() {
		_ = writeFrame(controlErrorResponse(frame.ID, "NOT_CONNECTED", "connect handshake required first", nil))
		return
	}

	// Throttle: block briefly if the per-connection in-flight cap is
	// hit. Studio's bootstrap fires ~6 in parallel so this normally
	// admits immediately.
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	go func() {
		defer func() { <-sem }()
		// Recover so one bad method handler doesn't kill the whole
		// connection. Surface to the client as INTERNAL_ERROR.
		defer func() {
			if rec := recover(); rec != nil {
				_ = writeFrame(controlErrorResponse(frame.ID, "INTERNAL_ERROR", fmt.Sprintf("handler panicked: %v", rec), nil))
			}
		}()
		s.dispatchControlMethod(ctx, frame, writeFrame)
	}()
}

// handleControlConnect runs the connect-handshake validation: protocol
// range check, token verification, scope-grant declaration. Mirrors the
// upstream openclaw connect contract. On success, signalConnected
// flips the per-connection `connected` flag AND closes the
// connectDone channel — atomically cancelling the handshake-timeout
// goroutine so it can't race with the success response.
func (s *Server) handleControlConnect(frame controlFrame, signalConnected func(), writeFrame func(controlFrame) error) {
	var params connectParams
	if len(frame.Params) > 0 {
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			_ = writeFrame(controlErrorResponse(frame.ID, "INVALID_REQUEST", "malformed connect params: "+err.Error(), nil))
			return
		}
	}

	// Protocol range check: we currently speak v3 only. Reject if the
	// client's [min, max] doesn't include 3.
	if params.MinProtocol > controlProtocolVersion || params.MaxProtocol < controlProtocolVersion {
		_ = writeFrame(controlErrorResponse(frame.ID, "PROTOCOL_VERSION_UNSUPPORTED",
			fmt.Sprintf("server speaks protocol %d; client requested [%d, %d]", controlProtocolVersion, params.MinProtocol, params.MaxProtocol),
			nil))
		return
	}

	// Token check: bypass when no token is configured (initial setup /
	// fully open gateway). Otherwise constant-time compare against
	// each configured auth value, matching the rules in isAuthorized.
	if !s.verifyControlToken(strings.TrimSpace(params.Auth.Token)) {
		_ = writeFrame(controlErrorResponse(frame.ID, "AUTH_REJECTED", "invalid or missing gateway token", nil))
		return
	}

	// Mark connected BEFORE writing the response so the timeout
	// goroutine cannot close the socket between the OK write and any
	// follow-up request. signalConnected is idempotent (sync.Once) so
	// it's safe even on retried connect flows.
	signalConnected()
	ok := true
	_ = writeFrame(controlFrame{
		Type: "res",
		ID:   frame.ID,
		OK:   &ok,
		Payload: map[string]any{
			"protocol": controlProtocolVersion,
			"scopes":   append([]string{}, controlScopes...),
			"caps":     append([]string{}, params.Caps...), // echo whatever the client asked for
			"server": map[string]any{
				"name":    "openclaw-go",
				"version": Version,
			},
		},
	})
}

// controlMethodHandler adapts an upstream openclaw method invocation to
// whatever combination of our existing dispatchRPC handlers + response
// shape adaptation it needs. It receives the request context (must be
// honored for any I/O) and the raw params (delegate decoding to the
// underlying dispatchRPC where possible). It returns the payload to
// send back, or an rpcError for failures.
//
// Authors of new handlers should prefer composition: call into
// s.dispatchRPC for an existing method when shapes are compatible, and
// only re-implement when the upstream protocol's expected shape
// differs from what openclaw-go's native RPC returns.
type controlMethodHandler func(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError)

// controlMethodHandlers is the dispatch table for upstream protocol
// requests after the connect handshake. Keys are method names studio
// (and other upstream-compatible clients) call. Adding a method here
// makes it reachable via /control/ws. Methods not listed return
// METHOD_NOT_FOUND.
//
// Phase 2 strategy: small adapters that call into dispatchRPC for the
// real work, plus light response-shape adaptation where upstream
// expects a different envelope. Methods that need substantial shape
// changes (config.get/set, status with heartbeat data, sessions.preview)
// are intentionally absent here — they'll land in Phase 3 with
// dedicated handlers.
var controlMethodHandlers = map[string]controlMethodHandler{
	"wake":        handleWake,
	"agents.list": handleAgentsList,
}

// dispatchControlMethod handles a request frame AFTER the connect
// handshake. Looks up the method in controlMethodHandlers; missing
// methods get a structured METHOD_NOT_FOUND so studio's adapter can
// surface the gap rather than hang on a missing response.
//
// ctx is the request context inherited from the HTTP request that
// upgraded into this WebSocket. Handlers that perform I/O MUST honor
// ctx so client disconnects propagate cancellation — otherwise a
// closed connection leaves blocked goroutines hanging on long-running
// work.
func (s *Server) dispatchControlMethod(ctx context.Context, frame controlFrame, writeFrame func(controlFrame) error) {
	handler, ok := controlMethodHandlers[frame.Method]
	if !ok {
		_ = writeFrame(controlErrorResponse(frame.ID, "METHOD_NOT_FOUND",
			"method not implemented on openclaw-go yet: "+frame.Method, nil))
		return
	}
	payload, rpcErr := handler(s, ctx, frame.Params)
	if rpcErr != nil {
		code, msg := mapRPCErrorToControl(rpcErr)
		_ = writeFrame(controlErrorResponse(frame.ID, code, msg, nil))
		return
	}
	okTrue := true
	_ = writeFrame(controlFrame{
		Type:    "res",
		ID:      frame.ID,
		OK:      &okTrue,
		Payload: payload,
	})
}

// mapRPCErrorToControl translates an internal rpcError (numeric
// JSON-RPC style codes) into the string code studio's adapter reads.
// Without this mapping, every adapter failure becomes a generic
// GATEWAY_ERROR — studio can't differentiate a missing-resource error
// from a malformed-params error from a transient backend failure, so
// the UI can't render a sensible message.
//
// JSON-RPC reference codes:
//
//	-32700 parse error           → INVALID_REQUEST
//	-32600 invalid request       → INVALID_REQUEST
//	-32601 method not found      → METHOD_NOT_FOUND
//	-32602 invalid params        → INVALID_REQUEST
//	-32603 internal error        → INTERNAL_ERROR
//	-32001 (our "not found" /
//	        operation-failed)    → NOT_FOUND
//	-32000 (our generic server)  → INTERNAL_ERROR
//	other                        → GATEWAY_ERROR (preserves message)
//
// The string codes match the vocabulary upstream openclaw uses on its
// own WS protocol (studio's gateway-connect-profile.ts switches on
// codes like INVALID_REQUEST / GATEWAY_UNAVAILABLE). Where we don't
// have a precise upstream analogue, GATEWAY_ERROR is the safe
// catchall.
func mapRPCErrorToControl(err *rpcError) (code, message string) {
	if err == nil {
		return "GATEWAY_ERROR", ""
	}
	switch err.Code {
	case -32700, -32600, -32602:
		return "INVALID_REQUEST", err.Message
	case -32601:
		return "METHOD_NOT_FOUND", err.Message
	case -32603, -32000:
		return "INTERNAL_ERROR", err.Message
	case -32001:
		return "NOT_FOUND", err.Message
	default:
		return "GATEWAY_ERROR", err.Message
	}
}

// handleWake is the heartbeat-like no-op studio fires to probe gateway
// liveness. Studio's lib/gateway/agentConfig.ts decodes the response as
// HeartbeatWakeResult so {ok, woke} are the load-bearing fields; we
// also return time for human debugging. Params are ignored — studio
// sometimes sends {mode: "now", text: "..."} to trigger an immediate
// heartbeat, but openclaw-go doesn't currently distinguish modes.
func handleWake(_ *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	// ctx unused: handler is synchronous. Named for the framework
	// contract — Phase 2+ handlers that perform I/O must honor it.
	_ = ctx
	return map[string]any{
		"ok":   true,
		"woke": true,
		"time": time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// handleAgentsList adapts our workspace.List() result into the shape
// studio's fleet hydration expects:
//
//	{ defaultId, mainKey, scope?, agents: [{id, name, identity?}, ...] }
//
// openclaw-go's native agents.list returns a flat `{agents: [...]}` of
// AgentProfile records. Studio additionally needs a defaultId (used to
// pick which agent is the "main" one on UI load) and a mainKey (used
// to scope session lookups). We synthesize:
//
//   - Agents sorted by CreatedAt (oldest first), then ID as tiebreaker
//     — Workspace.List() iterates a map so its native order is
//     non-deterministic; sorting here guarantees a stable response on
//     every call. Studio relies on defaultId being stable across
//     bootstraps, so this matters for correctness, not just polish.
//   - defaultId: the first (oldest) agent's id, or "main" if no agents
//     exist. "main" is the conventional name openclaw uses for the
//     primary agent in single-agent setups.
//   - mainKey: the literal string "main". openclaw-go uses one
//     namespace for sessions today; if/when we add session-scoping
//     this becomes a real value.
//
// We pass the full AgentProfile through inside `agents[]`; studio's
// shape only consumes id+name so extra fields are tolerated.
func handleAgentsList(s *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	// ctx unused: List() is in-memory + mutex-protected, can't block.
	// Named for the framework contract.
	_ = ctx
	list := s.workspace.List()
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
	defaultID := "main"
	if len(list) > 0 {
		defaultID = list[0].ID
	}
	return map[string]any{
		"defaultId": defaultID,
		"mainKey":   "main",
		"agents":    list,
	}, nil
}

// verifyControlToken compares the supplied token against every auth
// value the gateway is configured with. Returns true if any matches in
// constant time, or if no auth is configured (initial-setup mode).
func (s *Server) verifyControlToken(token string) bool {
	authToken, password, _ := s.authSnapshot()
	if authToken == "" && password == "" {
		// Open gateway: accept any token (including empty). Matches
		// the posture isAuthorized takes for the no-auth-configured
		// case on HTTP routes.
		return true
	}
	if token == "" {
		return false
	}
	if authToken != "" && constantTimeStringEqual(token, []byte(authToken)) {
		return true
	}
	if password != "" && constantTimeStringEqual(token, []byte(password)) {
		return true
	}
	return false
}

// controlErrorResponse builds a uniform error response frame. studio's
// adapter reads frame.error.code/message/details; missing fields are
// surfaced as defaults in the adapter so we always supply both.
func controlErrorResponse(id, code, message string, details any) controlFrame {
	ok := false
	return controlFrame{
		Type:  "res",
		ID:    id,
		OK:    &ok,
		Error: &controlError{Code: code, Message: message, Details: details},
	}
}
