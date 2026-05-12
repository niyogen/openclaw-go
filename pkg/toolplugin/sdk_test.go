package toolplugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadFromEnvHappyPath(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "weather-plugin")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://127.0.0.1:18789")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "tok-xyz")

	p, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "weather-plugin" {
		t.Errorf("name: %q", p.Name)
	}
	if p.GatewayURL != "http://127.0.0.1:18789" {
		t.Errorf("gateway: %q", p.GatewayURL)
	}
	if p.Token != "tok-xyz" {
		t.Errorf("token: %q", p.Token)
	}
}

func TestLoadFromEnvMissingVarsReportsAll(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "")
	t.Setenv("OPENCLAW_GATEWAY_URL", "")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "")
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error when env vars missing")
	}
	for _, v := range []string{"OPENCLAW_PLUGIN_NAME", "OPENCLAW_GATEWAY_URL", "OPENCLAW_PLUGIN_TOKEN"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("missing-var error should mention %s; got %v", v, err)
		}
	}
}

func TestRegisterToolAndTools(t *testing.T) {
	p := &Plugin{Name: "x", GatewayURL: "http://x", Token: "t"}
	p.RegisterTool("b", func(ctx context.Context, args map[string]any) (any, error) { return nil, nil })
	p.RegisterTool("a", func(ctx context.Context, args map[string]any) (any, error) { return nil, nil })
	got := p.Tools()
	want := []string{"a", "b"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Tools() = %v, want %v", got, want)
	}
}

func TestRegisterToolIgnoresEmptyName(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("", func(ctx context.Context, args map[string]any) (any, error) { return nil, nil })
	p.RegisterTool("  ", func(ctx context.Context, args map[string]any) (any, error) { return nil, nil })
	if len(p.Tools()) != 0 {
		t.Errorf("empty name should be rejected; got %v", p.Tools())
	}
}

func TestRegisterToolIgnoresNilHandler(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("x", nil)
	if len(p.Tools()) != 0 {
		t.Errorf("nil handler should be rejected; got %v", p.Tools())
	}
}

func TestRegisterToolLastWriteWins(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("a", func(ctx context.Context, args map[string]any) (any, error) { return "first", nil })
	p.RegisterTool("a", func(ctx context.Context, args map[string]any) (any, error) { return "second", nil })
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/tool/a", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Result string `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Result != "second" {
		t.Errorf("last-write-wins failed: result = %q", env.Result)
	}
}

func TestHandlerDispatchesByName(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("weather", func(ctx context.Context, args map[string]any) (any, error) {
		city, _ := args["city"].(string)
		return "sunny in " + city, nil
	})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	body := `{"city":"Colombo"}`
	resp, err := http.Post(srv.URL+"/tool/weather", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var env struct {
		Result string `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Result != "sunny in Colombo" {
		t.Errorf("result: %q", env.Result)
	}
}

func TestHandlerSurfacesHandlerError(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("flaky", func(ctx context.Context, args map[string]any) (any, error) {
		return nil, errors.New("upstream down")
	})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/tool/flaky", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: %d (want 500)", resp.StatusCode)
	}
	var env struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error != "upstream down" {
		t.Errorf("error envelope: %q", env.Error)
	}
}

func TestHandlerReturns404ForUnknownTool(t *testing.T) {
	p := &Plugin{}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/tool/nope", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: %d (want 404)", resp.StatusCode)
	}
}

func TestHandlerRejectsNonPOST(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("x", func(ctx context.Context, args map[string]any) (any, error) { return "ok", nil })
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/tool/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: %d (want 405)", resp.StatusCode)
	}
}

func TestHandlerAcceptsEmptyBody(t *testing.T) {
	p := &Plugin{}
	p.RegisterTool("now", func(ctx context.Context, args map[string]any) (any, error) {
		if args != nil {
			return nil, errors.New("expected nil args")
		}
		return "noon", nil
	})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/tool/now", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d", resp.StatusCode)
	}
	var env struct {
		Result string `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Result != "noon" {
		t.Errorf("empty-body case: %q", env.Result)
	}
}
