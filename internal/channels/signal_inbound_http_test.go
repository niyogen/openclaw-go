package channels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignalHTTPFetcherDecodesEnvelopes(t *testing.T) {
	const body = `[
	  {
	    "envelope": {
	      "source": "+15551111111",
	      "sourceName": "Alice",
	      "timestamp": 1700000000000,
	      "dataMessage": {
	        "message": "  hi from alice  ",
	        "groupInfo": null
	      }
	    }
	  },
	  {
	    "envelope": {
	      "source": "+15552222222",
	      "timestamp": 1700000000123,
	      "dataMessage": {
	        "message": "group reply",
	        "groupInfo": {"groupId": "GBASE64="}
	      }
	    }
	  }
	]`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 3*time.Second)
	msgs, err := f.FetchNew(context.Background())
	if err != nil {
		t.Fatalf("FetchNew: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if !strings.HasPrefix(gotPath, "/v1/receive/") || !strings.Contains(gotPath, "timeout=3") {
		t.Errorf("unexpected request path: %q", gotPath)
	}
	if msgs[0].Source != "+15551111111" || msgs[0].Message != "hi from alice" || msgs[0].GroupID != "" {
		t.Errorf("msg0 wrong: %+v", msgs[0])
	}
	if msgs[1].GroupID != "GBASE64=" || msgs[1].Message != "group reply" {
		t.Errorf("msg1 wrong: %+v", msgs[1])
	}
	if msgs[0].Timestamp != 1700000000000 {
		t.Errorf("msg0 timestamp lost: %d", msgs[0].Timestamp)
	}
}

func TestSignalHTTPFetcherFiltersNonDataMessages(t *testing.T) {
	// Receipts, typing indicators, and reactions all surface as
	// envelope.dataMessage=null (or empty message body). We must skip
	// them so the agent isn't dispatched on noise.
	const body = `[
	  {"envelope": {"source": "+1", "timestamp": 1}},
	  {"envelope": {"source": "+1", "timestamp": 2, "dataMessage": {"message": ""}}},
	  {"envelope": {"source": "+1", "timestamp": 3, "dataMessage": {"message": "real msg"}}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 2*time.Second)
	msgs, err := f.FetchNew(context.Background())
	if err != nil {
		t.Fatalf("FetchNew: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Message != "real msg" {
		t.Fatalf("want only the dataMessage with content, got %+v", msgs)
	}
}

func TestSignalHTTPFetcherEmptyBodyOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// signal-cli-rest-api sometimes returns 200 with an empty body
		// when the long-poll times out cleanly — must not be treated
		// as a decode error.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 2*time.Second)
	msgs, err := f.FetchNew(context.Background())
	if err != nil {
		t.Fatalf("empty body should be OK, got err: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("empty body → 0 messages, got %d", len(msgs))
	}
}

func TestSignalHTTPFetcherServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 2*time.Second)
	_, err := f.FetchNew(context.Background())
	if err == nil {
		t.Fatalf("expected error on 502, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestSignalHTTPFetcherMisconfigured(t *testing.T) {
	// Both empty: error fast so the poller's misconfiguration is loud.
	for _, tc := range []struct {
		name string
		base string
		num  string
	}{
		{"no base", "", "+15551234567"},
		{"no number", "http://127.0.0.1:8080", ""},
		{"both empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewSignalHTTPFetcher(tc.base, tc.num, time.Second)
			_, err := f.FetchNew(context.Background())
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestSignalHTTPFetcherCtxCancellation(t *testing.T) {
	// Server takes longer than the ctx deadline → the request must be
	// cancelled, not the HTTP client timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := f.FetchNew(ctx)
	if err == nil {
		t.Fatalf("expected ctx cancellation error, got nil")
	}
}

func TestSignalHTTPFetcherTimeoutClamping(t *testing.T) {
	// Under 1s → clamped up to 5s default; over 60s → clamped down to 60s.
	low := NewSignalHTTPFetcher("http://x", "+1", 0)
	if low.receiveTimeout != 5*time.Second {
		t.Errorf("zero timeout should clamp to 5s, got %s", low.receiveTimeout)
	}
	high := NewSignalHTTPFetcher("http://x", "+1", 120*time.Second)
	if high.receiveTimeout != 60*time.Second {
		t.Errorf("120s timeout should clamp to 60s, got %s", high.receiveTimeout)
	}
	ok := NewSignalHTTPFetcher("http://x", "+1", 10*time.Second)
	if ok.receiveTimeout != 10*time.Second {
		t.Errorf("10s timeout should pass through, got %s", ok.receiveTimeout)
	}
}

func TestSignalHTTPFetcherInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	f := NewSignalHTTPFetcher(srv.URL, "+15559999999", 2*time.Second)
	_, err := f.FetchNew(context.Background())
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode envelope") {
		t.Errorf("error should mention decode: %v", err)
	}
}
