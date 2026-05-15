package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openclaw-go/internal/agents"
)

// newTestAgent is a tiny constructor for AgentProfile in tests. Note
// that Workspace.Create overrides CreatedAt with time.Now() on every
// insertion — so call order in the test determines CreatedAt order,
// which handleAgentsList sorts by. Lexicographic ID tiebreaker
// guarantees deterministic order even on same-nanosecond ties.
func newTestAgent(id, name string) agents.AgentProfile {
	return agents.AgentProfile{ID: id, Name: name}
}

// ctlDial upgrades an httptest server URL to ws:// and connects to
// /control/ws. Returns the WebSocket conn; t.Cleanup closes it.
func ctlDial(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/control/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial /control/ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readFrame reads one frame and returns it. Errors fatal the test.
func readFrame(t *testing.T, conn *websocket.Conn) controlFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var f controlFrame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// writeReq sends a `req` frame with the given method + raw-json params.
func writeReq(t *testing.T, conn *websocket.Conn, id, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := conn.WriteJSON(controlFrame{Type: "req", ID: id, Method: method, Params: raw}); err != nil {
		t.Fatalf("write req: %v", err)
	}
}

// buildConnectParams returns the connect-params payload studio sends.
// Caller can override the token.
func buildConnectParams(token string) map[string]any {
	return map[string]any{
		"minProtocol": 3,
		"maxProtocol": 3,
		"role":        "operator",
		"scopes": []string{
			"operator.admin", "operator.read", "operator.write",
			"operator.approvals", "operator.pairing",
		},
		"caps": []string{"tool-events"},
		"auth": map[string]any{"token": token},
		"client": map[string]any{
			"id": "test-client", "version": "test", "platform": "node", "mode": "backend",
		},
	}
}

func TestControlWSConnectChallengeFires(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	f := readFrame(t, conn)
	if f.Type != "event" || f.Event != "connect.challenge" {
		t.Fatalf("expected event=connect.challenge, got type=%q event=%q", f.Type, f.Event)
	}
}

func TestControlWSConnectHappyPath_OpenGateway(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn) // challenge

	writeReq(t, conn, "1", "connect", buildConnectParams("")) // open gateway: empty token OK
	f := readFrame(t, conn)
	if f.Type != "res" || f.ID != "1" || f.OK == nil || !*f.OK {
		t.Fatalf("expected ok connect res, got %+v err=%+v", f, f.Error)
	}
	// Payload must carry protocol + all 5 scopes so studio doesn't
	// fall back to legacy-control-ui profile.
	payload, ok := f.Payload.(map[string]any)
	if !ok {
		t.Fatalf("expected map payload, got %T", f.Payload)
	}
	if payload["protocol"] != float64(controlProtocolVersion) {
		t.Errorf("protocol mismatch: %v", payload["protocol"])
	}
	scopes, _ := payload["scopes"].([]any)
	if len(scopes) != 5 {
		t.Errorf("expected 5 scopes, got %d (%v)", len(scopes), scopes)
	}
}

func TestControlWSConnectHappyPath_AuthedGateway(t *testing.T) {
	s := buildTestServer(t, "secret-token-xyz")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn) // challenge
	writeReq(t, conn, "c1", "connect", buildConnectParams("secret-token-xyz"))
	f := readFrame(t, conn)
	if f.OK == nil || !*f.OK {
		t.Fatalf("expected ok=true, got err=%+v", f.Error)
	}
}

func TestControlWSConnectRejectsBadToken(t *testing.T) {
	s := buildTestServer(t, "real-token")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	writeReq(t, conn, "c1", "connect", buildConnectParams("wrong-token"))
	f := readFrame(t, conn)
	if f.OK == nil || *f.OK {
		t.Fatalf("expected ok=false, got %+v", f)
	}
	if f.Error == nil || f.Error.Code != "AUTH_REJECTED" {
		t.Fatalf("expected AUTH_REJECTED error, got %+v", f.Error)
	}
}

func TestControlWSConnectRejectsBadProtocol(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	params := buildConnectParams("")
	params["minProtocol"] = 99
	params["maxProtocol"] = 99
	writeReq(t, conn, "c1", "connect", params)
	f := readFrame(t, conn)
	if f.OK == nil || *f.OK {
		t.Fatalf("expected ok=false, got %+v", f)
	}
	if f.Error == nil || f.Error.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
		t.Fatalf("expected PROTOCOL_VERSION_UNSUPPORTED, got %+v", f.Error)
	}
}

func TestControlWSConnectMalformedParams(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	// Send raw bytes that aren't a valid connect-params object.
	raw := json.RawMessage(`"not an object"`)
	if err := conn.WriteJSON(controlFrame{Type: "req", ID: "c1", Method: "connect", Params: raw}); err != nil {
		t.Fatal(err)
	}
	f := readFrame(t, conn)
	if f.Error == nil || f.Error.Code != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %+v", f.Error)
	}
}

func TestControlWSMethodCallBeforeConnect(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn) // challenge
	writeReq(t, conn, "x1", "wake", map[string]any{})
	f := readFrame(t, conn)
	if f.Error == nil || f.Error.Code != "NOT_CONNECTED" {
		t.Fatalf("expected NOT_CONNECTED, got %+v", f.Error)
	}
}

func TestControlWSDoubleConnectRejected(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	f1 := readFrame(t, conn)
	if f1.OK == nil || !*f1.OK {
		t.Fatalf("first connect failed: %+v", f1)
	}
	writeReq(t, conn, "c2", "connect", buildConnectParams(""))
	f2 := readFrame(t, conn)
	if f2.Error == nil || f2.Error.Code != "INVALID_REQUEST" {
		t.Fatalf("expected second connect rejected, got %+v", f2)
	}
}

func TestControlWSWakeAfterConnect(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	_ = readFrame(t, conn) // connect res

	writeReq(t, conn, "w1", "wake", map[string]any{})
	f := readFrame(t, conn)
	if f.Type != "res" || f.ID != "w1" || f.OK == nil || !*f.OK {
		t.Fatalf("wake failed: %+v", f)
	}
	payload, _ := f.Payload.(map[string]any)
	if got, _ := payload["woke"].(bool); !got {
		t.Errorf("expected woke=true, got %+v", payload)
	}
}

func TestControlWSUnknownMethodReturnsNotFound(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	_ = readFrame(t, conn)
	// Use a guaranteed-fake method name so the test stays valid as
	// the handler table grows. Earlier iterations used agents.list
	// then config.set as the probe; both got implemented and the
	// test broke. The leading "_not_" prefix is a reserved namespace
	// that real upstream openclaw methods never use.
	writeReq(t, conn, "x1", "_not_a_real_method", map[string]any{})
	f := readFrame(t, conn)
	if f.Error == nil || f.Error.Code != "METHOD_NOT_FOUND" {
		t.Fatalf("expected METHOD_NOT_FOUND, got %+v", f.Error)
	}
}

func TestControlWSConcurrentRequests(t *testing.T) {
	// Studio's bootstrap fires ~6 methods in parallel. We fire 40 —
	// deliberately MORE than controlMaxConcurrentRequests (32) so the
	// per-connection semaphore's wait-on-cap branch is exercised. With
	// 40 > 32 the dispatcher must block at least 8 acquires until
	// earlier handlers release their slots. The test passes if all 40
	// responses arrive without deadlock or dropped frames.
	if controlMaxConcurrentRequests < 32 {
		t.Skipf("test assumes default cap >= 32, got %d", controlMaxConcurrentRequests)
	}
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	_ = readFrame(t, conn)

	const n = 40
	var wg sync.WaitGroup
	received := make(map[string]bool, n)
	var rmu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for i := range n {
			var f controlFrame
			if err := conn.ReadJSON(&f); err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
			rmu.Lock()
			received[f.ID] = true
			rmu.Unlock()
		}
	}()

	for i := range n {
		writeReq(t, conn, fmt.Sprintf("p%d", i), "wake", map[string]any{})
	}
	wg.Wait()
	if len(received) != n {
		t.Fatalf("expected %d responses, got %d (%v)", n, len(received), received)
	}
	// Verify each id we sent came back exactly once.
	for i := range n {
		if !received[fmt.Sprintf("p%d", i)] {
			t.Errorf("missing response for id p%d", i)
		}
	}
}

func TestControlWSHandshakeTimeoutClosesConn(t *testing.T) {
	// Bare upgrade with no `req method=connect` follow-up: server
	// must close the connection within ~controlConnectTimeout so an
	// unauthed client can't camp on the socket.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn) // challenge
	// Don't send anything; wait for the server to close.
	_ = conn.SetReadDeadline(time.Now().Add(controlConnectTimeout + 2*time.Second))
	for {
		_, _, err := conn.NextReader()
		if err != nil {
			return // expected: connection closed
		}
	}
}

func TestControlWSNonReqFramesIgnored(t *testing.T) {
	// Client sends a stray `event` frame (which a well-behaved client
	// never does, but the server must not crash on garbage).
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	if err := conn.WriteJSON(controlFrame{Type: "event", Event: "garbage"}); err != nil {
		t.Fatal(err)
	}
	// Then send a valid connect — should still work.
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	f := readFrame(t, conn)
	if f.OK == nil || !*f.OK {
		t.Fatalf("expected connect to still succeed: %+v", f)
	}
}

// TestControlWSThroughTracedMux exercises /control/ws through the
// production handler chain (mux wrapped in tracedMux + statusRecorder).
// All other tests serve from s.mux directly, which bypasses the trace
// middleware — so this test is the only automated coverage that the
// statusRecorder Hijack/Unwrap forwarders actually work. Before the
// fix in trace.go, this test fails with HTTP 500 at WebSocket upgrade.
func TestControlWSThroughTracedMux(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.withTrace(s.mux))
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/control/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial through tracedMux: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	challenge := readFrame(t, conn)
	if challenge.Event != "connect.challenge" {
		t.Fatalf("expected challenge through tracedMux, got %+v", challenge)
	}
	writeReq(t, conn, "c1", "connect", buildConnectParams(""))
	res := readFrame(t, conn)
	if res.OK == nil || !*res.OK {
		t.Fatalf("connect through tracedMux failed: %+v", res.Error)
	}
}

func TestControlWSEmptyIDRejected(t *testing.T) {
	// Empty-id requests can never pair with a pending client response
	// — they'd hang the client. Server rejects them loudly.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := ctlDial(t, ts)
	_ = readFrame(t, conn)
	if err := conn.WriteJSON(controlFrame{Type: "req", Method: "connect"}); err != nil {
		t.Fatal(err)
	}
	f := readFrame(t, conn)
	if f.Error == nil || f.Error.Code != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST on empty id, got %+v", f.Error)
	}
	if strings.Contains(f.Error.Message, "method is required") {
		t.Fatalf("id-check should fire before method-check; got %q", f.Error.Message)
	}
}

// connectFor returns a freshly-handshaked /control/ws connection on the
// supplied test server. Reads challenge, sends connect, reads ok.
// Tests that exercise post-connect methods should use this to skip the
// handshake boilerplate.
func connectFor(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn := ctlDial(t, ts)
	_ = readFrame(t, conn) // challenge
	writeReq(t, conn, "c0", "connect", buildConnectParams(token))
	res := readFrame(t, conn)
	if res.OK == nil || !*res.OK {
		t.Fatalf("connect failed: %+v", res.Error)
	}
	return conn
}

func TestControlWSAgentsListEmpty(t *testing.T) {
	// Empty workspace: defaultId falls back to "main", mainKey is
	// always "main" in the openclaw-go scoping model.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := connectFor(t, ts, "")
	writeReq(t, conn, "a1", "agents.list", map[string]any{})
	f := readFrame(t, conn)
	if f.OK == nil || !*f.OK {
		t.Fatalf("agents.list failed: %+v", f.Error)
	}
	payload, ok := f.Payload.(map[string]any)
	if !ok {
		t.Fatalf("expected map payload, got %T", f.Payload)
	}
	if payload["defaultId"] != "main" {
		t.Errorf("expected defaultId=main on empty workspace, got %v", payload["defaultId"])
	}
	if payload["mainKey"] != "main" {
		t.Errorf("expected mainKey=main, got %v", payload["mainKey"])
	}
	agentsField, ok := payload["agents"].([]any)
	if !ok {
		t.Fatalf("expected agents to be array, got %T", payload["agents"])
	}
	if len(agentsField) != 0 {
		t.Errorf("expected empty agents list, got %d entries", len(agentsField))
	}
}

func TestControlWSAgentsListWithAgents(t *testing.T) {
	// Populated workspace: ordering is sorted-by-CreatedAt-asc with
	// ID as the tiebreaker, so the FIRST-created agent (alpha here)
	// is always first in the response and is the defaultId. This
	// pins what was previously a non-deterministic map-iteration
	// behavior. The time.Sleep guarantees the two Create calls land
	// on different CreatedAt values even on coarse-resolution clocks;
	// without it, alpha < beta lexicographic happens to align with
	// chronological order, so the test would pass for the wrong reason.
	// (See TestControlWSAgentsListSortsByCreationOrder for the
	// isolated chronological-vs-lexicographic regression test.)
	s := buildTestServer(t, "")
	if err := s.workspace.Create(newTestAgent("alpha", "Alpha agent")); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.workspace.Create(newTestAgent("beta", "Beta agent")); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Hit the endpoint twice and require identical responses — the
	// regression test for the bug where map-iteration returned
	// different orderings on successive calls.
	conn := connectFor(t, ts, "")
	captureAgentsList := func(t *testing.T, id string) (string, []string) {
		t.Helper()
		writeReq(t, conn, id, "agents.list", map[string]any{})
		f := readFrame(t, conn)
		if f.OK == nil || !*f.OK {
			t.Fatalf("agents.list failed: %+v", f.Error)
		}
		payload, _ := f.Payload.(map[string]any)
		defaultID, _ := payload["defaultId"].(string)
		agentsField, _ := payload["agents"].([]any)
		ids := make([]string, 0, len(agentsField))
		for _, a := range agentsField {
			m, _ := a.(map[string]any)
			id, _ := m["id"].(string)
			ids = append(ids, id)
		}
		return defaultID, ids
	}

	defaultID1, ids1 := captureAgentsList(t, "a1")
	defaultID2, ids2 := captureAgentsList(t, "a2")

	// alpha was created first → alpha is the default and appears first.
	if defaultID1 != "alpha" {
		t.Errorf("expected defaultId=alpha (oldest), got %q", defaultID1)
	}
	if len(ids1) != 2 || ids1[0] != "alpha" || ids1[1] != "beta" {
		t.Errorf("expected [alpha, beta] ordering, got %v", ids1)
	}
	// Stability across successive calls.
	if defaultID1 != defaultID2 {
		t.Errorf("defaultId not stable: %q vs %q", defaultID1, defaultID2)
	}
	if len(ids1) != len(ids2) {
		t.Errorf("agent count not stable")
	} else {
		for i := range ids1 {
			if ids1[i] != ids2[i] {
				t.Errorf("agents[%d] not stable: %q vs %q", i, ids1[i], ids2[i])
			}
		}
	}
}

func TestControlWSAgentsListSortsByCreationOrder(t *testing.T) {
	// Regression test for the determinism fix: create agents in REVERSE
	// alphabetical order with a real time gap between them. If the
	// handler sorted by ID instead of CreatedAt, the response would be
	// [alpha, zeta] with defaultId=alpha. With CreatedAt-asc sort,
	// the response is [zeta, alpha] with defaultId=zeta — matching the
	// claim in the godoc that "oldest agent is the default" rather than
	// "alphabetically first agent is the default." This isolates the
	// chronological-vs-lexicographic path that the alpha/beta test
	// can't distinguish.
	s := buildTestServer(t, "")
	if err := s.workspace.Create(newTestAgent("zeta", "Zeta agent (created first)")); err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.workspace.Create(newTestAgent("alpha", "Alpha agent (created second)")); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := connectFor(t, ts, "")
	writeReq(t, conn, "a1", "agents.list", map[string]any{})
	f := readFrame(t, conn)
	if f.OK == nil || !*f.OK {
		t.Fatalf("agents.list failed: %+v", f.Error)
	}
	payload, _ := f.Payload.(map[string]any)
	defaultID, _ := payload["defaultId"].(string)
	if defaultID != "zeta" {
		t.Errorf("expected defaultId=zeta (chronologically first), got %q — "+
			"handler may be sorting by ID instead of CreatedAt", defaultID)
	}
	agentsField, _ := payload["agents"].([]any)
	if len(agentsField) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agentsField))
	}
	first, _ := agentsField[0].(map[string]any)
	second, _ := agentsField[1].(map[string]any)
	if first["id"] != "zeta" || second["id"] != "alpha" {
		t.Errorf("expected [zeta, alpha], got [%v, %v]", first["id"], second["id"])
	}
}

func TestMapRPCErrorToControl(t *testing.T) {
	// Verify each rpcError.Code maps to the expected upstream string
	// code. Studio's adapter routes UI behavior on the string code, so
	// each mapping is load-bearing for error UX.
	cases := []struct {
		name string
		in   *rpcError
		code string
	}{
		{"nil error", nil, "GATEWAY_ERROR"},
		{"parse error", &rpcError{Code: -32700, Message: "parse"}, "INVALID_REQUEST"},
		{"invalid request", &rpcError{Code: -32600, Message: "bad"}, "INVALID_REQUEST"},
		{"invalid params", &rpcError{Code: -32602, Message: "bad params"}, "INVALID_REQUEST"},
		{"method not found", &rpcError{Code: -32601, Message: "no method"}, "METHOD_NOT_FOUND"},
		{"internal error -32603", &rpcError{Code: -32603, Message: "boom"}, "INTERNAL_ERROR"},
		{"server error -32000", &rpcError{Code: -32000, Message: "fail"}, "INTERNAL_ERROR"},
		{"not found -32001", &rpcError{Code: -32001, Message: "agent missing"}, "NOT_FOUND"},
		{"unknown code", &rpcError{Code: 999, Message: "?"}, "GATEWAY_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := mapRPCErrorToControl(tc.in)
			if got != tc.code {
				t.Errorf("code: got %q, want %q", got, tc.code)
			}
			if tc.in != nil && msg != tc.in.Message {
				t.Errorf("message not preserved: got %q, want %q", msg, tc.in.Message)
			}
		})
	}
}

func TestControlWSMethodNotFoundShapeStable(t *testing.T) {
	// A method explicitly absent from controlMethodHandlers must
	// return METHOD_NOT_FOUND so a misconfigured client gets a
	// debuggable error rather than a silent drop.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	conn := connectFor(t, ts, "")
	writeReq(t, conn, "u1", "this.is.definitely.not.a.method", map[string]any{})
	f := readFrame(t, conn)
	if f.Error == nil || f.Error.Code != "METHOD_NOT_FOUND" {
		t.Fatalf("expected METHOD_NOT_FOUND, got %+v", f.Error)
	}
	if !strings.Contains(f.Error.Message, "this.is.definitely.not.a.method") {
		t.Errorf("error message should include method name; got %q", f.Error.Message)
	}
}

func TestControlWSFanoutGoroutineExitsOnDisconnect(t *testing.T) {
	// Regression test for the leak: fanoutControlEvents was started
	// with r.Context() as its cancel signal, but hijacked WS conns
	// detach from the http server's request context — r.Context()
	// is not cancelled when the handler returns. Fixed via an
	// explicit handlerDone channel.
	//
	// This test verifies the fix indirectly by counting goroutines
	// before and after a series of connect+disconnect cycles. If
	// the leak is back, count grows linearly with cycles.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Settle.
	time.Sleep(50 * time.Millisecond)
	base := goroutineCount()

	const cycles = 20
	for i := range cycles {
		conn := ctlDial(t, ts)
		_ = readFrame(t, conn) // challenge
		writeReq(t, conn, fmt.Sprintf("c%d", i), "connect", buildConnectParams(""))
		_ = readFrame(t, conn) // connect res — triggers fanout spawn
		_ = conn.Close()
	}
	// Allow goroutines to wind down.
	time.Sleep(300 * time.Millisecond)
	after := goroutineCount()
	delta := after - base

	// Some noise is expected (test infra, http server pool). With
	// the leak the count grows by ~cycles (one per disconnected
	// session). With the fix it should stay within single digits.
	if delta > cycles/2 {
		t.Errorf("suspected goroutine leak: %d cycles, baseline %d, after %d (delta %d)",
			cycles, base, after, delta)
	}
}

// goroutineCount returns the current number of running goroutines.
// Use only as a smoke-level leak detector — counts include test
// infrastructure goroutines so the absolute number is noisy; the
// useful signal is the *delta* across an operation.
func goroutineCount() int {
	return runtime.NumGoroutine()
}

func TestControlWSAllowsNoOriginHeader(t *testing.T) {
	// Studio's server-side WS adapter doesn't send Origin. Ensure we
	// don't reject the upgrade on empty Origin.
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/control/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	// Default Dialer omits Origin (gorilla doesn't set it by default).
	conn, resp, err := dialer.Dial(u.String(), http.Header{})
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	defer conn.Close()
	f := readFrame(t, conn)
	if f.Event != "connect.challenge" {
		t.Fatalf("expected challenge, got %+v", f)
	}
}
