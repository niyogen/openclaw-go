package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// (strings is already imported)

func TestMatrixSendHappyPath(t *testing.T) {
	var seenMethod, seenPath, seenAuth, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	t.Cleanup(srv.Close)

	ch := NewMatrixChannel(srv.URL, "syt_abc123")
	err := ch.Send(context.Background(), OutboundMessage{
		Target:  "!roomA:example.com",
		Message: "hello matrix",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenMethod != http.MethodPut {
		t.Fatalf("method: got %s want PUT", seenMethod)
	}
	if seenAuth != "Bearer syt_abc123" {
		t.Fatalf("auth header: %q", seenAuth)
	}
	// Path is PathEscaped — the ! becomes %21 and : becomes %3A.
	if !strings.HasPrefix(seenPath, "/_matrix/client/v3/rooms/") {
		t.Fatalf("path prefix wrong: %s", seenPath)
	}
	if !strings.Contains(seenPath, "/send/m.room.message/") {
		t.Fatalf("path missing send segment: %s", seenPath)
	}
	var got map[string]string
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got["msgtype"] != "m.text" || got["body"] != "hello matrix" {
		t.Fatalf("body: %+v", got)
	}
}

func TestMatrixDisabledWhenURLEmpty(t *testing.T) {
	ch := NewMatrixChannel("", "tok")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "!r:e.com", Message: "hi"}); err != nil {
		t.Fatalf("disabled channel should return nil; got %v", err)
	}
}

func TestMatrixDisabledWhenTokenEmpty(t *testing.T) {
	ch := NewMatrixChannel("http://example.com", "")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "!r:e.com", Message: "hi"}); err != nil {
		t.Fatalf("disabled channel should return nil; got %v", err)
	}
}

func TestMatrixRejectsAlias(t *testing.T) {
	ch := NewMatrixChannel("http://example.com", "tok")
	err := ch.Send(context.Background(), OutboundMessage{Target: "#general:example.com", Message: "hi"})
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("expected alias-rejection error, got %v", err)
	}
}

func TestMatrixServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	ch := NewMatrixChannel(srv.URL, "tok")
	err := ch.Send(context.Background(), OutboundMessage{Target: "!r:e.com", Message: "hi"})
	if err == nil {
		t.Fatal("expected error from 403")
	}
}

func TestMatrixRespectsCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ch := NewMatrixChannel(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ch.Send(ctx, OutboundMessage{Target: "!r:example.com", Message: "hi"})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("error should mention context: %v", err)
	}
}

func TestMatrixTxnIDsAreUnique(t *testing.T) {
	ch := NewMatrixChannel("http://example.com", "tok")
	first := ch.nextTxnID()
	second := ch.nextTxnID()
	if first == second {
		t.Fatalf("txn ids must differ: %q vs %q", first, second)
	}
}
