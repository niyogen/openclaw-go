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

func TestSignalSendHappyPath(t *testing.T) {
	var seenURL, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ch := NewSignalChannel(srv.URL, "+15550001111")
	err := ch.Send(context.Background(), OutboundMessage{
		Target:  "+15552223333",
		Message: "hello signal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenURL != "/v2/send" {
		t.Fatalf("unexpected path: %s", seenURL)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(seenBody), &got); err != nil {
		t.Fatal(err)
	}
	if got["message"] != "hello signal" {
		t.Fatalf("message: %v", got["message"])
	}
	if got["number"] != "+15550001111" {
		t.Fatalf("number: %v", got["number"])
	}
	recipients, _ := got["recipients"].([]any)
	if len(recipients) != 1 || recipients[0] != "+15552223333" {
		t.Fatalf("recipients: %v", recipients)
	}
}

func TestSignalDisabledWhenURLEmpty(t *testing.T) {
	ch := NewSignalChannel("", "+15550001111")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "+15552223333", Message: "hi"}); err != nil {
		t.Fatalf("expected nil for disabled channel, got %v", err)
	}
}

func TestSignalDisabledWhenNumberEmpty(t *testing.T) {
	ch := NewSignalChannel("http://localhost:8080", "")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "+15552223333", Message: "hi"}); err != nil {
		t.Fatalf("expected nil for disabled channel, got %v", err)
	}
}

func TestSignalMissingTargetErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ch := NewSignalChannel(srv.URL, "+15550001111")
	if err := ch.Send(context.Background(), OutboundMessage{Message: "no target"}); err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestSignalServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "captcha required", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	ch := NewSignalChannel(srv.URL, "+15550001111")
	err := ch.Send(context.Background(), OutboundMessage{
		Target:  "+15552223333",
		Message: "hello",
	})
	if err == nil {
		t.Fatal("expected error from 4xx response")
	}
}

func TestSignalRespectsCancelledContext(t *testing.T) {
	// Server that hangs forever — only ctx cancellation can unstick the client.
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // serves until the *server* sees cancellation
		_ = hang             // satisfy unused if we change behaviour later
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hang) })

	ch := NewSignalChannel(srv.URL, "+15550001111")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Send so the http call returns immediately

	err := ch.Send(ctx, OutboundMessage{Target: "+15552223333", Message: "hi"})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("error should mention context cancellation; got %v", err)
	}
}

func TestSignalTrimsTrailingSlashInBaseURL(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ch := NewSignalChannel(srv.URL+"/", "+15550001111")
	_ = ch.Send(context.Background(), OutboundMessage{Target: "+15552223333", Message: "hi"})
	if seenPath != "/v2/send" {
		t.Fatalf("expected single /v2/send, got %q", seenPath)
	}
}
