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

func TestMattermostSendHappyPath(t *testing.T) {
	var seenPath, seenAuth, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	ch := NewMattermostChannel(srv.URL, "tok-abc")
	err := ch.Send(context.Background(), OutboundMessage{
		Target:  "channel-id-26",
		Message: "hello mm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/api/v4/posts" {
		t.Fatalf("path: %s", seenPath)
	}
	if seenAuth != "Bearer tok-abc" {
		t.Fatalf("auth: %s", seenAuth)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got["channel_id"] != "channel-id-26" || got["message"] != "hello mm" {
		t.Fatalf("body: %+v", got)
	}
	if _, hasRoot := got["root_id"]; hasRoot {
		t.Fatalf("root_id should be absent when no thread; got %+v", got)
	}
}

func TestMattermostSendThreadIncludesRootID(t *testing.T) {
	var seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	ch := NewMattermostChannel(srv.URL, "tok")
	_ = ch.Send(context.Background(), OutboundMessage{
		Target:   "c1",
		Message:  "reply",
		ThreadID: "root-post-id",
	})
	var got map[string]any
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got["root_id"] != "root-post-id" {
		t.Fatalf("root_id missing/wrong: %+v", got)
	}
}

func TestMattermostDisabledWhenURLEmpty(t *testing.T) {
	ch := NewMattermostChannel("", "tok")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "c1", Message: "hi"}); err != nil {
		t.Fatalf("disabled channel returns nil; got %v", err)
	}
}

func TestMattermostDisabledWhenTokenEmpty(t *testing.T) {
	ch := NewMattermostChannel("http://example.com", "")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "c1", Message: "hi"}); err != nil {
		t.Fatalf("disabled channel returns nil; got %v", err)
	}
}

func TestMattermostMissingChannelErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	ch := NewMattermostChannel(srv.URL, "tok")
	if err := ch.Send(context.Background(), OutboundMessage{Message: "hi"}); err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestMattermostRespectsCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ch := NewMattermostChannel(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ch.Send(ctx, OutboundMessage{Target: "c1", Message: "hi"})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("error should mention context: %v", err)
	}
}

func TestMattermostServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"id":"api.context.permissions.app_error"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	ch := NewMattermostChannel(srv.URL, "tok")
	if err := ch.Send(context.Background(), OutboundMessage{Target: "c1", Message: "hi"}); err == nil {
		t.Fatal("expected error from 403")
	}
}
