package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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
	writeReq(t, conn, "x1", "agents.list", map[string]any{})
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
