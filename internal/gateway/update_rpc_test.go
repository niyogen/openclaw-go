package gateway

import (
	"context"
	"testing"
)

func TestDispatchRPCUpdateStatusUsesLatestReleaseCheckFn(t *testing.T) {
	prev := LatestReleaseCheckFn
	defer func() { LatestReleaseCheckFn = prev }()
	LatestReleaseCheckFn = func(ctx context.Context) (string, string, error) {
		return "v2.0.0", "https://example.invalid/releases", nil
	}

	prevVer := Version
	Version = "1.0.0"
	defer func() { Version = prevVer }()

	s := buildTestServer(t, "")
	out, rpcErr := s.dispatchRPC(context.Background(), "update.status", nil)
	if rpcErr != nil {
		t.Fatalf("rpc err %+v", rpcErr)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("want map got %T", out)
	}
	if m["latestVersion"] != "v2.0.0" {
		t.Fatalf("latestVersion %+v", m["latestVersion"])
	}
	if m["releasesPage"] != "https://example.invalid/releases" {
		t.Fatalf("releasesPage %+v", m["releasesPage"])
	}
	if m["currentVersion"] != "1.0.0" {
		t.Fatalf("currentVersion %+v", m["currentVersion"])
	}
	if avail, _ := m["updateAvailable"].(bool); !avail {
		t.Fatalf("updateAvailable want true got %v", m["updateAvailable"])
	}
}

func TestDispatchRPCUpdateStatusCheckErrorFromFn(t *testing.T) {
	prev := LatestReleaseCheckFn
	defer func() { LatestReleaseCheckFn = prev }()
	LatestReleaseCheckFn = func(ctx context.Context) (string, string, error) {
		return "", "", context.Canceled
	}

	s := buildTestServer(t, "")
	out, rpcErr := s.dispatchRPC(context.Background(), "update.status", nil)
	if rpcErr != nil {
		t.Fatalf("rpc err %+v", rpcErr)
	}
	m := out.(map[string]any)
	if m["checkError"] == nil || m["checkError"] == "" {
		t.Fatalf("expected checkError, got %#v", m)
	}
}
