package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSemverCmp(t *testing.T) {
	if semverCmp("1.0.0", "1.0.1") >= 0 {
		t.Fatal("1.0.0 < 1.0.1")
	}
	if semverCmp("1.0.1", "1.0.0") <= 0 {
		t.Fatal("1.0.1 > 1.0.0")
	}
	if semverCmp("1.0.0", "1.0.0") != 0 {
		t.Fatal("equal")
	}
	if semverCmp("0.9.0", "0.10.0") >= 0 {
		t.Fatal("0.9 < 0.10")
	}
}

func TestUpdateAvailable(t *testing.T) {
	if !UpdateAvailable("1.0.0", "v1.0.1") {
		t.Fatal("expected update")
	}
	if UpdateAvailable("1.0.1", "v1.0.0") {
		t.Fatal("no downgrade")
	}
	if UpdateAvailable("dev", "v9.9.9") {
		t.Fatal("dev should not auto-update")
	}
	if UpdateAvailable("", "v1.0.0") {
		t.Fatal("empty current")
	}
	if UpdateAvailable("1.0.0", "v1.0.0") {
		t.Fatal("same version should not update")
	}
}

func TestFetchGitHubLatestRelease_MockOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","html_url":"https://example.com/release"}`))
	}))
	defer srv.Close()

	tag, page, err := fetchGitHubLatestRelease(context.Background(), srv.URL, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v2.0.0" {
		t.Fatalf("tag %q", tag)
	}
	if page != "https://example.com/release" {
		t.Fatalf("page %q", page)
	}
}

func TestFetchGitHubLatestRelease_MockFallbackPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.1.0","html_url":""}`))
	}))
	defer srv.Close()

	_, page, err := fetchGitHubLatestRelease(context.Background(), srv.URL, "acme", "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/acme/demo/releases/latest"
	if page != want {
		t.Fatalf("page %q want %q", page, want)
	}
}

func TestFetchGitHubLatestRelease_MockHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()

	_, _, err := fetchGitHubLatestRelease(context.Background(), srv.URL, "o", "r")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "github api") {
		t.Fatalf("err %v", err)
	}
}

func TestFetchGitHubLatestRelease_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	_, _, err := fetchGitHubLatestRelease(context.Background(), srv.URL, "o", "r")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchGitHubTagList_Mock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"v0.5.0"},{"name":"v0.4.0"}]`))
	}))
	defer srv.Close()

	tag, page, err := fetchGitHubTagList(context.Background(), srv.URL, "acme", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.5.0" {
		t.Fatalf("tag %q", tag)
	}
	wantPage := "https://github.com/acme/demo/tags"
	if page != wantPage {
		t.Fatalf("page %q", page)
	}
}
