package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus()

	ch, unsub := bus.Subscribe("")
	defer unsub()

	bus.Publish(GatewayEvent{Type: EventSessionCreated, SessionID: "s1"})
	bus.Publish(GatewayEvent{Type: EventSessionMessage, SessionID: "s1", Data: "hello"})

	var received []GatewayEvent
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			received = append(received, ev)
			if len(received) == 2 {
				goto done
			}
		case <-deadline:
			goto done
		}
	}
done:
	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}
}

func TestEventBusSessionFilter(t *testing.T) {
	bus := NewEventBus()

	ch, unsub := bus.Subscribe("target-session")
	defer unsub()

	bus.Publish(GatewayEvent{Type: EventSessionMessage, SessionID: "other-session"})
	bus.Publish(GatewayEvent{Type: EventSessionMessage, SessionID: "target-session"})

	select {
	case ev := <-ch:
		if ev.SessionID != "target-session" {
			t.Fatalf("expected target-session, got %s", ev.SessionID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive expected event")
	}

	select {
	case ev := <-ch:
		t.Fatalf("should not have received other-session event: %v", ev)
	case <-time.After(20 * time.Millisecond):
		// correct — no spurious event
	}
}

func TestSessionsSubscribeRPC(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Send a message to populate the bus.
	go func() {
		time.Sleep(20 * time.Millisecond)
		http.Post(ts.URL+"/message", "application/json", //nolint:errcheck
			bytes.NewBufferString(`{"sessionId":"bus-test","message":"hi","channel":"cli"}`))
	}()

	body := `{"jsonrpc":"2.0","id":1,"method":"sessions.subscribe","params":{"sessionId":"bus-test"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["error"] != nil {
		t.Fatalf("rpc error: %v", envelope["error"])
	}
}

func TestMessagePublishesEvent(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	ch, unsub := s.bus.Subscribe("event-sess")
	defer unsub()

	http.Post(ts.URL+"/message", "application/json", //nolint:errcheck
		bytes.NewBufferString(`{"sessionId":"event-sess","message":"test event","channel":"cli"}`))

	select {
	case ev := <-ch:
		if ev.SessionID != "event-sess" {
			t.Fatalf("wrong session id: %s", ev.SessionID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event received within 500ms")
	}
}
