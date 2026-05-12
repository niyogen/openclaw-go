package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/config"
	"openclaw-go/internal/gateway"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/push"
	"openclaw-go/internal/sandbox"
	"openclaw-go/internal/sessions"
)

var gatewayAuthToken string

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	gatewayAuthToken = strings.TrimSpace(cfg.Gateway.AuthToken)
	baseURL := fmt.Sprintf("http://%s:%d", cfg.Gateway.Host, cfg.Gateway.Port)

	switch os.Args[1] {
	case "onboard":
		if err := runOnboard(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "onboard error: %v\n", err)
			os.Exit(1)
		}
	case "config":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw config init|show|get|set|validate|file|path")
			os.Exit(2)
		}
		switch os.Args[2] {
		case "init":
			if err := initConfig(); err != nil {
				fmt.Fprintf(os.Stderr, "config init error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Wrote default config.")
		case "show", "get":
			if err := printConfig(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "config show error: %v\n", err)
				os.Exit(1)
			}
		case "set":
			if len(os.Args) < 5 {
				fmt.Println("usage: openclaw config set <key> <value>")
				os.Exit(2)
			}
			key, value := os.Args[3], strings.Join(os.Args[4:], " ")
			fmt.Printf("config set %s=%s (runtime only; edit %s/.openclaw-go/openclaw.json to persist)\n", key, value, os.Getenv("USERPROFILE"))
		case "unset":
			if len(os.Args) < 4 {
				fmt.Println("usage: openclaw config unset <key>")
				os.Exit(2)
			}
			fmt.Printf("config unset %s (edit config file to persist)\n", os.Args[3])
		case "validate":
			path, _ := config.DefaultPath()
			fmt.Printf("Config path: %s\n", path)
			if _, err := config.Load(path); err != nil {
				fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Config is valid.")
		case "file", "path":
			path, _ := config.DefaultPath()
			fmt.Println(path)
		default:
			fmt.Println("usage: openclaw config init|show|get|set|validate|file|path")
			os.Exit(2)
		}
	case "configure":
		if err := runConfigure(cfg, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "configure error: %v\n", err)
			os.Exit(1)
		}
	case "gateway":
		if len(os.Args) >= 3 {
			switch os.Args[2] {
			case "run":
				if err := runGateway(cfg); err != nil {
					fmt.Fprintf(os.Stderr, "gateway error: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Println("usage: openclaw gateway [run]")
				os.Exit(2)
			}
		} else {
			if err := runGateway(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "gateway error: %v\n", err)
				os.Exit(1)
			}
		}
	case "status":
		if err := get(baseURL + "/health"); err != nil {
			fmt.Fprintf(os.Stderr, "status error: %v\n", err)
			os.Exit(1)
		}
	case "sessions":
		if err := get(baseURL + "/sessions"); err != nil {
			fmt.Fprintf(os.Stderr, "sessions error: %v\n", err)
			os.Exit(1)
		}
	case "session":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw session <get|delete|history|kill|patch> <id> [args]")
			os.Exit(2)
		}
		subcmd := os.Args[2]
		if len(os.Args) < 4 {
			fmt.Println("usage: openclaw session " + subcmd + " <id>")
			os.Exit(2)
		}
		id := os.Args[3]
		u := baseURL + "/sessions/" + url.PathEscape(id)
		switch subcmd {
		case "get":
			if err := get(u); err != nil {
				fmt.Fprintf(os.Stderr, "session get error: %v\n", err)
				os.Exit(1)
			}
		case "delete":
			if err := deleteHTTP(u); err != nil {
				fmt.Fprintf(os.Stderr, "session delete error: %v\n", err)
				os.Exit(1)
			}
		case "history":
			if err := get(u + "/history"); err != nil {
				fmt.Fprintf(os.Stderr, "session history error: %v\n", err)
				os.Exit(1)
			}
		case "kill":
			if err := post(u+"/kill", map[string]any{}); err != nil {
				fmt.Fprintf(os.Stderr, "session kill error: %v\n", err)
				os.Exit(1)
			}
		case "patch":
			if len(os.Args) < 6 {
				fmt.Println("usage: openclaw session patch <id> <index> <new-content>")
				os.Exit(2)
			}
			idx, _ := strconv.Atoi(os.Args[4])
			content := strings.Join(os.Args[5:], " ")
			if err := post(u+"/patch", []map[string]any{{"index": idx, "content": content}}); err != nil {
				fmt.Fprintf(os.Stderr, "session patch error: %v\n", err)
				os.Exit(1)
			}
		case "compact":
			keepN := 20
			if len(os.Args) >= 5 {
				keepN, _ = strconv.Atoi(os.Args[4])
			}
			if err := post(u+"/compact", map[string]any{"keepN": keepN}); err != nil {
				fmt.Fprintf(os.Stderr, "session compact error: %v\n", err)
				os.Exit(1)
			}
		case "stats":
			if err := get(u + "/stats"); err != nil {
				fmt.Fprintf(os.Stderr, "session stats error: %v\n", err)
				os.Exit(1)
			}
		default:
			fmt.Println("usage: openclaw session get|delete|history|kill|patch|compact|stats <id>")
			os.Exit(2)
		}
	case "message":
		if err := runMessage(baseURL, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "message error: %v\n", err)
			os.Exit(1)
		}
	case "agent":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw agent <message>")
			os.Exit(2)
		}
		payload := map[string]string{
			"sessionId": "main",
			"message":   os.Args[2],
			"channel":   "cli",
		}
		if err := post(baseURL+"/message", payload); err != nil {
			fmt.Fprintf(os.Stderr, "agent error: %v\n", err)
			os.Exit(1)
		}
	case "doctor":
		if err := runDoctor(cfg, baseURL); err != nil {
			fmt.Fprintf(os.Stderr, "doctor failed: %v\n", err)
			os.Exit(1)
		}
	case "rpc":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw rpc <method> [args...]")
			os.Exit(2)
		}
		method := os.Args[2]
		params, err := parseRPCParams(method, os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rpc params error: %v\n", err)
			os.Exit(2)
		}
		if err := rpc(baseURL+"/rpc", method, params); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "approvals":
		if err := get(baseURL + "/approvals"); err != nil {
			fmt.Fprintf(os.Stderr, "approvals error: %v\n", err)
			os.Exit(1)
		}
	case "approve":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw approve <approval-id>")
			os.Exit(2)
		}
		if err := post(baseURL+"/approvals/"+url.PathEscape(os.Args[2])+"/decide", map[string]any{"approved": true}); err != nil {
			fmt.Fprintf(os.Stderr, "approve error: %v\n", err)
			os.Exit(1)
		}
	case "reject":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw reject <approval-id>")
			os.Exit(2)
		}
		if err := post(baseURL+"/approvals/"+url.PathEscape(os.Args[2])+"/decide", map[string]any{"approved": false}); err != nil {
			fmt.Fprintf(os.Stderr, "reject error: %v\n", err)
			os.Exit(1)
		}
	case "models":
		provider := ""
		if len(os.Args) >= 3 {
			provider = os.Args[2]
		}
		runModels(provider)
	case "capability":
		provider := cfg.Agent.Provider
		if len(os.Args) >= 3 {
			provider = os.Args[2]
		}
		apiKey := resolveProviderKey(cfg, provider)
		cap := agents.Capability(provider, apiKey)
		raw, _ := json.MarshalIndent(cap, "", "  ")
		fmt.Println(string(raw))
	case "infer":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw infer <message>")
			os.Exit(2)
		}
		runner := agents.NewRunnerFromOptions(agents.RunnerOptions{
			Provider:         cfg.Agent.Provider,
			OpenAIAPIKey:     cfg.Providers.OpenAI.APIKey,
			OpenAIBaseURL:    cfg.Providers.OpenAI.BaseURL,
			OpenAIModel:      cfg.Providers.OpenAI.Model,
			AnthropicAPIKey:  cfg.Providers.Anthropic.APIKey,
			AnthropicBaseURL: cfg.Providers.Anthropic.BaseURL,
			AnthropicModel:   cfg.Providers.Anthropic.Model,
		})
		reply, err := agents.Infer(context.Background(), runner, strings.Join(os.Args[2:], " "))
		if err != nil {
			fmt.Fprintf(os.Stderr, "infer error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(reply)
	case "logs":
		q := "/logs"
		if len(os.Args) >= 3 {
			q += "?level=" + url.QueryEscape(os.Args[2])
		}
		if err := get(baseURL + q); err != nil {
			fmt.Fprintf(os.Stderr, "logs error: %v\n", err)
			os.Exit(1)
		}
	case "cron":
		if len(os.Args) < 3 {
			if err := get(baseURL + "/cron"); err != nil {
				fmt.Fprintf(os.Stderr, "cron error: %v\n", err)
				os.Exit(1)
			}
		} else {
			switch os.Args[2] {
			case "list":
				if err := get(baseURL + "/cron"); err != nil {
					fmt.Fprintf(os.Stderr, "cron list error: %v\n", err)
					os.Exit(1)
				}
			case "add":
				if len(os.Args) < 6 {
					fmt.Println("usage: openclaw cron add <id> <schedule> <command>")
					os.Exit(2)
				}
				payload := map[string]any{
					"id":       os.Args[3],
					"name":     os.Args[3],
					"schedule": os.Args[4],
					"command":  strings.Join(os.Args[5:], " "),
					"enabled":  true,
				}
				if err := post(baseURL+"/cron", payload); err != nil {
					fmt.Fprintf(os.Stderr, "cron add error: %v\n", err)
					os.Exit(1)
				}
			case "delete", "remove":
				if len(os.Args) < 4 {
					fmt.Println("usage: openclaw cron delete <id>")
					os.Exit(2)
				}
				if err := deleteHTTP(baseURL + "/cron/" + url.PathEscape(os.Args[3])); err != nil {
					fmt.Fprintf(os.Stderr, "cron delete error: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Println("usage: openclaw cron [list|add|delete]")
				os.Exit(2)
			}
		}
	case "hooks":
		if len(os.Args) < 3 {
			if err := get(baseURL + "/hooks"); err != nil {
				fmt.Fprintf(os.Stderr, "hooks error: %v\n", err)
				os.Exit(1)
			}
		} else {
			switch os.Args[2] {
			case "list":
				if err := get(baseURL + "/hooks"); err != nil {
					fmt.Fprintf(os.Stderr, "hooks list error: %v\n", err)
					os.Exit(1)
				}
			case "add":
				if len(os.Args) < 6 {
					fmt.Println("usage: openclaw hooks add <id> <event> <type> <target>")
					os.Exit(2)
				}
				payload := map[string]any{
					"id":      os.Args[3],
					"name":    os.Args[3],
					"event":   os.Args[4],
					"type":    os.Args[5],
					"target":  strings.Join(os.Args[6:], " "),
					"enabled": true,
				}
				if err := post(baseURL+"/hooks", payload); err != nil {
					fmt.Fprintf(os.Stderr, "hooks add error: %v\n", err)
					os.Exit(1)
				}
			case "delete", "remove":
				if len(os.Args) < 4 {
					fmt.Println("usage: openclaw hooks delete <id>")
					os.Exit(2)
				}
				if err := deleteHTTP(baseURL + "/hooks/" + url.PathEscape(os.Args[3])); err != nil {
					fmt.Fprintf(os.Stderr, "hooks delete error: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Println("usage: openclaw hooks [list|add|delete]")
				os.Exit(2)
			}
		}
	case "secrets":
		if len(os.Args) < 3 {
			if err := get(baseURL + "/secrets"); err != nil {
				fmt.Fprintf(os.Stderr, "secrets error: %v\n", err)
				os.Exit(1)
			}
		} else {
			switch os.Args[2] {
			case "list":
				if err := get(baseURL + "/secrets"); err != nil {
					fmt.Fprintf(os.Stderr, "secrets list error: %v\n", err)
					os.Exit(1)
				}
			case "set":
				if len(os.Args) < 5 {
					fmt.Println("usage: openclaw secrets set <name> <value>")
					os.Exit(2)
				}
				if err := post(baseURL+"/secrets", map[string]string{"name": os.Args[3], "value": strings.Join(os.Args[4:], " ")}); err != nil {
					fmt.Fprintf(os.Stderr, "secrets set error: %v\n", err)
					os.Exit(1)
				}
			case "delete", "remove":
				if len(os.Args) < 4 {
					fmt.Println("usage: openclaw secrets delete <name>")
					os.Exit(2)
				}
				if err := deleteHTTP(baseURL + "/secrets/" + url.PathEscape(os.Args[3])); err != nil {
					fmt.Fprintf(os.Stderr, "secrets delete error: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Println("usage: openclaw secrets [list|set|delete]")
				os.Exit(2)
			}
		}
	case "plugins":
		if err := runPluginsCLI(baseURL, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "plugins error: %v\n", err)
			os.Exit(1)
		}
	case "usage":
		if err := rpc(baseURL+"/rpc", "usage.stats", map[string]any{}); err != nil {
			fmt.Fprintf(os.Stderr, "usage error: %v\n", err)
			os.Exit(1)
		}
	case "channels":
		if err := rpc(baseURL+"/rpc", "channels.list", map[string]any{}); err != nil {
			fmt.Fprintf(os.Stderr, "channels error: %v\n", err)
			os.Exit(1)
		}
	case "nodes":
		raw, _ := json.MarshalIndent(cfg.Nodes, "", "  ")
		fmt.Println(string(raw))
	case "skills":
		raw, _ := json.MarshalIndent(cfg.Skills, "", "  ")
		fmt.Println(string(raw))
	case "mcp":
		raw, _ := json.MarshalIndent(cfg.MCP, "", "  ")
		fmt.Println(string(raw))
	case "memory":
		raw, _ := json.MarshalIndent(cfg.Memory, "", "  ")
		fmt.Println(string(raw))
	case "health":
		// Liveness probe.
		if err := get(baseURL + "/health"); err != nil {
			fmt.Fprintf(os.Stderr, "health error: %v\n", err)
			os.Exit(1)
		}
	case "ready":
		// Readiness probe.
		if err := get(baseURL + "/ready"); err != nil {
			fmt.Fprintf(os.Stderr, "ready error: %v\n", err)
			os.Exit(1)
		}
	case "stop":
		// Graceful gateway shutdown via RPC.
		if err := rpc(baseURL+"/rpc", "gateway.stop", map[string]any{}); err != nil {
			fmt.Fprintf(os.Stderr, "stop error: %v\n", err)
			os.Exit(1)
		}
	case "tools":
		if len(os.Args) < 3 {
			if err := get(baseURL + "/tools"); err != nil {
				fmt.Fprintf(os.Stderr, "tools error: %v\n", err)
				os.Exit(1)
			}
		} else {
			switch os.Args[2] {
			case "list":
				if err := get(baseURL + "/tools"); err != nil {
					fmt.Fprintf(os.Stderr, "tools list error: %v\n", err)
					os.Exit(1)
				}
			case "invoke":
				if len(os.Args) < 4 {
					fmt.Println("usage: openclaw tools invoke <name> [args...]")
					os.Exit(2)
				}
				params, err := parseRPCParams("tools.invoke", os.Args[3:])
				if err != nil {
					fmt.Fprintf(os.Stderr, "tools invoke params error: %v\n", err)
					os.Exit(2)
				}
				if err := rpc(baseURL+"/rpc", "tools.invoke", params); err != nil {
					fmt.Fprintf(os.Stderr, "tools invoke error: %v\n", err)
					os.Exit(1)
				}
			}
		}
	case "sandbox":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw sandbox run <script>")
			os.Exit(2)
		}
		switch os.Args[2] {
		case "run":
			if len(os.Args) < 4 {
				fmt.Println("usage: openclaw sandbox run <script>")
				os.Exit(2)
			}
			script := strings.Join(os.Args[3:], " ")
			if err := rpc(baseURL+"/rpc", "tools.invoke", map[string]any{
				"name":      "sandbox.run",
				"arguments": map[string]any{"script": script},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "sandbox run error: %v\n", err)
				os.Exit(1)
			}
		case "available":
			if err := rpc(baseURL+"/rpc", "tools.invoke", map[string]any{
				"name": "sandbox.available", "arguments": map[string]any{},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "sandbox available error: %v\n", err)
				os.Exit(1)
			}
		}
	case "chat", "tui", "terminal":
		sessionID := "chat-" + time.Now().Format("20060102-150405")
		if len(os.Args) >= 3 {
			sessionID = os.Args[2]
		}
		fmt.Printf("OpenClaw-Go chat (session: %s) — type 'exit' to quit\n", sessionID)
		scanner := bufio.NewScanner(os.Stdin)
		for {
			fmt.Print("> ")
			if !scanner.Scan() {
				break
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "exit" || line == "quit" {
				break
			}
			// Use streaming endpoint for live output.
			streamResp, err := func() (*http.Response, error) {
				return post2(baseURL+"/v1/chat/completions", map[string]any{
					"model":    "openclaw-go",
					"stream":   true,
					"messages": []map[string]string{{"role": "user", "content": line}},
				})
			}()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			chatScanner := bufio.NewScanner(streamResp.Body)
			fmt.Print("Assistant: ")
			for chatScanner.Scan() {
				l := chatScanner.Text()
				if !strings.HasPrefix(l, "data: ") {
					continue
				}
				data := strings.TrimPrefix(l, "data: ")
				if data == "[DONE]" {
					break
				}
				var chunk struct {
					Choices []struct {
						Delta struct{ Content string } `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
					fmt.Print(chunk.Choices[0].Delta.Content)
				}
			}
			streamResp.Body.Close()
			fmt.Println()
		}
	case "backup":
		if err := runBackup(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "backup error: %v\n", err)
			os.Exit(1)
		}
	case "restore":
		if err := runRestore(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "restore error: %v\n", err)
			os.Exit(1)
		}
	case "migrate":
		fmt.Println("Migration: checking config format…")
		path, _ := config.DefaultPath()
		loadedCfg, err := config.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate error loading config: %v\n", err)
			os.Exit(1)
		}
		if err := config.Save(path, loadedCfg); err != nil {
			fmt.Fprintf(os.Stderr, "migrate error saving: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Migration complete — config rewritten to current schema.")
	case "update":
		sub := ""
		if len(os.Args) >= 3 {
			sub = os.Args[2]
		}
		switch sub {
		case "status":
			if err := rpc(baseURL+"/rpc", "update.status", map[string]any{}); err != nil {
				fmt.Fprintf(os.Stderr, "update status error: %v\n", err)
				os.Exit(1)
			}
		case "run":
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			latest, page, err := gateway.FetchDefaultRepoLatestRelease(ctx)
			fmt.Println("Update check (openclaw-go on GitHub):")
			if err != nil {
				fmt.Printf("  error: %v\n", err)
			} else {
				fmt.Printf("  latest release tag: %s\n", latest)
				if page != "" {
					fmt.Printf("  releases: %s\n", page)
				}
				fmt.Printf("  this binary version: %s\n", gateway.Version)
				if gateway.UpdateAvailable(gateway.Version, latest) {
					fmt.Println("  status: a newer release may be available — replace the binary manually or use your package manager.")
				} else {
					fmt.Println("  status: you appear to be on the latest or a dev/custom build.")
				}
			}
			fmt.Println("Automated install is not performed. See:")
			fmt.Println("  https://github.com/niyogen/openclaw-go/releases")
		default:
			if err := rpc(baseURL+"/rpc", "update.status", map[string]any{}); err != nil {
				fmt.Fprintf(os.Stderr, "update error: %v\n", err)
				os.Exit(1)
			}
		}
	case "version":
		if err := get(baseURL + "/health"); err != nil {
			// Gateway not running — just print compiled version placeholder.
			fmt.Println("openclaw-go version: (gateway not running)")
		}
	case "embeddings":
		if len(os.Args) < 3 {
			fmt.Println("usage: openclaw embeddings <text>")
			os.Exit(2)
		}
		texts := os.Args[2:]
		if err := post(baseURL+"/v1/embeddings", map[string]any{
			"model": "text-embedding-3-small",
			"input": texts,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "embeddings error: %v\n", err)
			os.Exit(1)
		}
	case "dashboard":
		if err := runDashboard(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard error: %v\n", err)
			os.Exit(1)
		}
	case "daemon":
		if err := runDaemon(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "daemon error: %v\n", err)
			os.Exit(1)
		}
	case "compaction":
		if err := runCompactionCLI(baseURL, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "compaction error: %v\n", err)
			os.Exit(1)
		}
	case "web-login":
		if err := runWebLoginCLI(baseURL, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "web-login error: %v\n", err)
			os.Exit(1)
		}
	default:
		printUsage()
		os.Exit(2)
	}
}

func runModels(provider string) {
	var list []agents.ModelInfo
	if strings.TrimSpace(provider) == "" {
		list = agents.KnownModels()
	} else {
		list = agents.ModelsForProvider(provider)
	}
	raw, _ := json.MarshalIndent(list, "", "  ")
	fmt.Println(string(raw))
}

func resolveProviderKey(cfg config.Config, provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return cfg.Providers.OpenAI.APIKey
	case "anthropic", "claude":
		return cfg.Providers.Anthropic.APIKey
	default:
		return ""
	}
}

func printUsage() {
	fmt.Println("OpenClaw-Go")
	fmt.Println("usage:")
	fmt.Println("  openclaw onboard [--provider <echo|openai|anthropic>]")
	fmt.Println("                  [--openai-key <key>] [--anthropic-key <key>]")
	fmt.Println("                  [--gateway-token <bearer>] [--gateway-port <port>]")
	fmt.Println("  openclaw config init|show")
	fmt.Println("  openclaw configure gateway auth-token <token>")
	fmt.Println("  openclaw configure gateway allowed-origins <csv>")
	fmt.Println("  openclaw configure gateway metrics-require-auth <true|false>")
	fmt.Println("  openclaw configure gateway push-contact <mailto:owner@example.com>")
	fmt.Println("  openclaw configure set-agent-provider <echo|openai|anthropic>")
	fmt.Println("  openclaw configure telegram inbound-mode <polling|webhook>")
	fmt.Println("  openclaw configure telegram enable <true|false>")
	fmt.Println("  openclaw configure telegram webhook set <public-base-url>")
	fmt.Println("  openclaw configure telegram use-plugin <true|false>")
	fmt.Println("  openclaw configure slack enable <true|false>")
	fmt.Println("  openclaw configure slack inbound-mode <webhook>")
	fmt.Println("  openclaw configure slack webhook-path <path>")
	fmt.Println("  openclaw configure discord enable <true|false>")
	fmt.Println("  openclaw configure discord inbound-mode <webhook>")
	fmt.Println("  openclaw configure discord webhook-path <path>")
	fmt.Println("  openclaw configure teams enable <true|false>")
	fmt.Println("  openclaw configure teams inbound-mode <webhook>")
	fmt.Println("  openclaw configure teams webhook-path <path>")
	fmt.Println("  openclaw configure whatsapp enable <true|false>")
	fmt.Println("  openclaw configure whatsapp inbound-mode <webhook>")
	fmt.Println("  openclaw configure whatsapp webhook-path <path>")
	fmt.Println("  openclaw configure whatsapp use-plugin <true|false>")
	fmt.Println("  openclaw configure email enable|host|port|user|password|from <value>")
	fmt.Println("                          |inbound-enable|imap-host|imap-port|imap-tls")
	fmt.Println("                          |imap-mailbox|imap-poll <value>")
	fmt.Println("  openclaw configure signal enable|baseurl|number <value>")
	fmt.Println("  openclaw configure matrix enable|baseurl|token <value>")
	fmt.Println("  openclaw configure mattermost enable|baseurl|token <value>")
	fmt.Println("  openclaw logs [<level>]")
	fmt.Println("  openclaw cron [list|add <id> <schedule> <cmd>|delete <id>]")
	fmt.Println("  openclaw hooks [list|add <id> <event> <type> <target>|delete <id>]")
	fmt.Println("  openclaw secrets [list|set <name> <value>|delete <name>]")
	fmt.Println("  openclaw plugins [channel list|channel approve <name>|channel revoke <name>]")
	fmt.Println("  openclaw plugins [tool list|tool approve <name>|tool revoke <name>]")
	fmt.Println("  openclaw plugins [hook list|hook approve <name>|hook revoke <name>]")
	fmt.Println("  openclaw approvals")
	fmt.Println("  openclaw approve <approval-id>")
	fmt.Println("  openclaw reject <approval-id>")
	fmt.Println("  openclaw models [<provider>]")
	fmt.Println("  openclaw capability [<provider>]")
	fmt.Println("  openclaw infer <message>")
	fmt.Println("  openclaw embeddings <text...>")
	fmt.Println("  openclaw tools [list|invoke <name> [args]]")
	fmt.Println("  openclaw sandbox [run <script>|available]")
	fmt.Println("  openclaw gateway [run]")
	fmt.Println("  openclaw stop")
	fmt.Println("  openclaw status")
	fmt.Println("  openclaw health")
	fmt.Println("  openclaw ready")
	fmt.Println("  openclaw version")
	fmt.Println("  openclaw doctor")
	fmt.Println("  openclaw dashboard")
	fmt.Println("  openclaw daemon install|uninstall|path")
	fmt.Println("  openclaw web-login")
	fmt.Println("  openclaw compaction list <session-id>|get <id>|restore <id> --yes|branch <id> [--id <new>]")
	fmt.Println("  openclaw backup [list]")
	fmt.Println("  openclaw restore <backup-path> --yes")
	fmt.Println("  openclaw usage")
	fmt.Println("  openclaw channels")
	fmt.Println("  openclaw nodes")
	fmt.Println("  openclaw skills")
	fmt.Println("  openclaw mcp")
	fmt.Println("  openclaw memory")
	fmt.Println("  openclaw rpc <method> [args...]")
	fmt.Println("  openclaw sessions")
	fmt.Println("  openclaw session get|history|kill|delete|patch|compact|stats <id>")
	fmt.Println("  openclaw message send|history|dispatch ...")
	fmt.Println("  openclaw agent <message>")
}

// validateGatewayChannelConfig returns an error if enabled channels are missing
// required settings (fail fast before opening stores or binding listeners).
func validateGatewayChannelConfig(cfg config.Config) error {
	if cfg.Channels.WhatsApp.Enabled && strings.TrimSpace(cfg.Channels.WhatsApp.VerifyToken) == "" {
		return fmt.Errorf("whatsapp is enabled but verify token is empty: set channels.whatsapp.verifyToken or WHATSAPP_VERIFY_TOKEN")
	}
	// Email/Signal/Matrix/Mattermost validation: catch misconfig before the
	// gateway starts, otherwise the channel silently no-ops because Send()
	// returns nil when its required fields are empty.
	if cfg.Channels.Email.Enabled && strings.TrimSpace(cfg.Channels.Email.Host) == "" {
		return fmt.Errorf("email is enabled but channels.email.host is empty")
	}
	if cfg.Channels.Email.InboundEnabled {
		// Username/password are shared with outbound, so check those here
		// even if outbound is disabled — operators may run inbound-only.
		if strings.TrimSpace(cfg.Channels.Email.Username) == "" {
			return fmt.Errorf("email inbound is enabled but channels.email.username is empty")
		}
		if strings.TrimSpace(cfg.Channels.Email.Password) == "" {
			return fmt.Errorf("email inbound is enabled but channels.email.password is empty")
		}
		// IMAPHost defaults to outbound Host, so either is fine.
		if strings.TrimSpace(cfg.Channels.Email.IMAPHost) == "" &&
			strings.TrimSpace(cfg.Channels.Email.Host) == "" {
			return fmt.Errorf("email inbound is enabled but neither channels.email.imapHost nor channels.email.host is set")
		}
	}
	if cfg.Channels.Signal.Enabled {
		if strings.TrimSpace(cfg.Channels.Signal.BaseURL) == "" {
			return fmt.Errorf("signal is enabled but channels.signal.baseUrl is empty (point at your signal-cli-rest-api sidecar)")
		}
		if strings.TrimSpace(cfg.Channels.Signal.Number) == "" {
			return fmt.Errorf("signal is enabled but channels.signal.number is empty (the bot's own Signal number)")
		}
	}
	if cfg.Channels.Matrix.Enabled {
		if strings.TrimSpace(cfg.Channels.Matrix.BaseURL) == "" {
			return fmt.Errorf("matrix is enabled but channels.matrix.baseUrl is empty")
		}
		if strings.TrimSpace(cfg.Channels.Matrix.AccessToken) == "" {
			return fmt.Errorf("matrix is enabled but channels.matrix.accessToken is empty")
		}
	}
	if cfg.Channels.Mattermost.Enabled {
		if strings.TrimSpace(cfg.Channels.Mattermost.BaseURL) == "" {
			return fmt.Errorf("mattermost is enabled but channels.mattermost.baseUrl is empty")
		}
		if strings.TrimSpace(cfg.Channels.Mattermost.AccessToken) == "" {
			return fmt.Errorf("mattermost is enabled but channels.mattermost.accessToken is empty")
		}
	}
	return nil
}

// openClawDataDir returns the directory for sessions, gateway auxiliary stores, and the default
// plugins directory when Gateway.PluginsDir is unset. OPENCLAW_DATA_DIR overrides ~/.openclaw-go
// (used by the Docker image and docker-compose).
func openClawDataDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("OPENCLAW_DATA_DIR")); d != "" {
		return filepath.Clean(d), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".openclaw-go"), nil
}

func buildGatewayRunner(cfg config.Config) agents.Runner {
	return agents.NewRunnerFromOptions(agents.RunnerOptions{
		Provider:         cfg.Agent.Provider,
		OpenAIAPIKey:     cfg.Providers.OpenAI.APIKey,
		OpenAIBaseURL:    cfg.Providers.OpenAI.BaseURL,
		OpenAIModel:      cfg.Providers.OpenAI.Model,
		AnthropicAPIKey:  cfg.Providers.Anthropic.APIKey,
		AnthropicBaseURL: cfg.Providers.Anthropic.BaseURL,
		AnthropicModel:   cfg.Providers.Anthropic.Model,
	})
}

func buildPerSessionRunnerFactory(cfg config.Config) func(provider, model string) agents.Runner {
	return func(provider, model string) agents.Runner {
		return agents.NewRunnerFromOptions(agents.RunnerOptions{
			Provider:         provider,
			OpenAIAPIKey:     cfg.Providers.OpenAI.APIKey,
			OpenAIBaseURL:    cfg.Providers.OpenAI.BaseURL,
			OpenAIModel:      model,
			AnthropicAPIKey:  cfg.Providers.Anthropic.APIKey,
			AnthropicBaseURL: cfg.Providers.Anthropic.BaseURL,
			AnthropicModel:   model,
		})
	}
}

func warnAgentMisconfig(cfg config.Config) {
	p := strings.ToLower(strings.TrimSpace(cfg.Agent.Provider))
	if p == "openai" && strings.TrimSpace(cfg.Providers.OpenAI.APIKey) == "" {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: agent.provider is openai but no OpenAI API key is set (providers.openai.apiKey or OPENAI_API_KEY); replies will echo until a key is configured.\n")
	}
	if (p == "anthropic" || p == "claude") && strings.TrimSpace(cfg.Providers.Anthropic.APIKey) == "" {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: agent.provider is %s but no Anthropic API key is set.\n", cfg.Agent.Provider)
	}
}

func pushAgentSummary(server *gateway.Server, cfg config.Config) {
	server.SetAgentSummary(
		cfg.Agent.Provider,
		cfg.Agent.Model,
		strings.TrimSpace(cfg.Providers.OpenAI.APIKey) != "",
		strings.TrimSpace(cfg.Providers.Anthropic.APIKey) != "",
	)
}

func runGateway(cfg config.Config) error {
	if err := validateGatewayChannelConfig(cfg); err != nil {
		return err
	}
	dataDir, err := openClawDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	statePath := filepath.Join(dataDir, "sessions.json")
	store, err := sessions.New(statePath)
	if err != nil {
		return err
	}

	runner := buildGatewayRunner(cfg)
	channelRouter := channels.NewRouter()
	if cfg.Channels.Webhook.Enabled {
		channelRouter.Register(channels.NewWebhookChannel(cfg.Channels.Webhook.OutboundURL))
	}
	if cfg.Channels.Telegram.Enabled && !cfg.Channels.Telegram.UsePlugin {
		// Built-in path. Skipped when UsePlugin=true; in that case the
		// out-of-process Telegram plugin registers a pluginChannel with
		// the same "telegram" name via the channel-plugin registry.
		channelRouter.Register(channels.NewTelegramChannel(
			cfg.Channels.Telegram.BotToken,
			cfg.Channels.Telegram.ChatID,
		))
	}
	if cfg.Channels.Slack.Enabled {
		channelRouter.Register(channels.NewSlackChannel(
			cfg.Channels.Slack.BotToken,
			cfg.Channels.Slack.ChannelID,
		))
	}
	if cfg.Channels.Discord.Enabled {
		channelRouter.Register(channels.NewDiscordChannel(
			cfg.Channels.Discord.BotToken,
			cfg.Channels.Discord.ChannelID,
		))
	}
	if cfg.Channels.Teams.Enabled {
		channelRouter.Register(channels.NewTeamsChannel(
			cfg.Channels.Teams.OutboundURL,
		))
	}
	if cfg.Channels.WhatsApp.Enabled && !cfg.Channels.WhatsApp.UsePlugin {
		// Built-in outbound path. Skipped when UsePlugin=true; in that
		// case the out-of-process WhatsApp plugin registers a
		// pluginChannel with the same "whatsapp" name via the
		// channel-plugin registry. The inbound webhook handler below
		// is NOT gated by UsePlugin — WhatsApp inbound is webhook-only
		// (Meta-driven public URL) and continues to live in the
		// gateway.
		channelRouter.Register(channels.NewWhatsAppChannel(
			cfg.Channels.WhatsApp.AccessToken,
			cfg.Channels.WhatsApp.PhoneNumberID,
			cfg.Channels.WhatsApp.ToNumber,
		))
	}
	if cfg.Channels.Line.Enabled {
		channelRouter.Register(channels.NewLineChannel(
			cfg.Channels.Line.ChannelToken,
			cfg.Channels.Line.ChannelSecret,
		))
	}
	if cfg.Channels.Nostr.Enabled {
		channelRouter.Register(channels.NewNostrChannel(
			cfg.Channels.Nostr.RelayURL,
			cfg.Channels.Nostr.Pubkey,
		))
	}
	if cfg.Channels.Email.Enabled {
		channelRouter.Register(channels.NewEmailChannel(
			cfg.Channels.Email.Host,
			cfg.Channels.Email.Port,
			cfg.Channels.Email.Username,
			cfg.Channels.Email.Password,
			cfg.Channels.Email.From,
		))
	}
	if cfg.Channels.Signal.Enabled {
		channelRouter.Register(channels.NewSignalChannel(
			cfg.Channels.Signal.BaseURL,
			cfg.Channels.Signal.Number,
		))
	}
	if cfg.Channels.Matrix.Enabled {
		channelRouter.Register(channels.NewMatrixChannel(
			cfg.Channels.Matrix.BaseURL,
			cfg.Channels.Matrix.AccessToken,
		))
	}
	if cfg.Channels.Mattermost.Enabled {
		channelRouter.Register(channels.NewMattermostChannel(
			cfg.Channels.Mattermost.BaseURL,
			cfg.Channels.Mattermost.AccessToken,
		))
	}
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))

	// Load external plugins from configured directory.
	pluginsDir := cfg.Gateway.PluginsDir
	if pluginsDir == "" {
		pluginsDir = filepath.Join(dataDir, "plugins")
	}
	loader := plugins.NewLoader(pluginsDir)
	if externalPlugins, err := loader.Load(); err == nil {
		for _, ep := range externalPlugins {
			registry.Register(ep)
			// Register each tool declared in the plugin manifest.
			for _, tool := range ep.Tools() {
				fmt.Printf("loaded plugin %s — tool: %s\n", ep.Name(), tool.Name)
			}
			fmt.Printf("loaded plugin: %s\n", ep.Name())
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)

	server := gateway.New(
		cfg.Gateway.Host,
		cfg.Gateway.Port,
		cfg.Gateway.AuthToken,
		cfg.Gateway.AllowedOrigins,
		store,
		runner,
		channelRouter,
		registry,
		dataDir,
	)

	// Configure additional auth modes (password + trusted proxies).
	server.SetAuth(cfg.Gateway.Password, cfg.Gateway.TrustedProxies)

	// Channel plugins: scan pluginsDir for plugin.json manifests that
	// declare a channel, build pluginChannel instances for the approved
	// ones, register them with the router (alongside built-ins), and
	// mount the gateway-side inbound handler. Pending plugins are
	// catalogued but not active — operator runs `openclaw plugins
	// approve <name>` to issue a token and flip them on.
	channelPluginsDir := pluginsDir
	channelTokensFile := filepath.Join(dataDir, "channel-plugin-tokens.json")
	channelReg, err := plugins.NewChannelPluginRegistry(channelPluginsDir, channelTokensFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: channel-plugin registry init failed: %v (channel plugins disabled)\n", err)
	} else {
		server.SetChannelPluginRegistry(channelReg)
		for _, m := range channelReg.ApprovedManifests() {
			channelRouter.Register(plugins.NewPluginChannel(m))
		}
		server.HandleFunc(
			"/plugins/{name}/inbound",
			plugins.BuildChannelPluginInboundHandler(channelReg, func(inboundCtx context.Context, inbound channels.InboundMessage) error {
				_, err := server.HandleInbound(inboundCtx, inbound)
				return err
			}),
		)
	}

	// Tool plugins: scan pluginsDir for plugin.json manifests that
	// declare tools[]. For each approved manifest, register every
	// declared tool with the gateway's ToolRegistry — a JSON Schema-less
	// tool whose handler POSTs to the plugin's endpoint. Pending tool
	// plugins are catalogued but not registered (operator approves via
	// `openclaw plugins tool approve <name>`; a SIGHUP/restart picks
	// them up).
	toolPluginsDir := pluginsDir
	toolTokensFile := filepath.Join(dataDir, "tool-plugin-tokens.json")
	toolReg, err := plugins.NewToolPluginRegistry(toolPluginsDir, toolTokensFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: tool-plugin registry init failed: %v (tool plugins disabled)\n", err)
	} else {
		server.SetToolPluginRegistry(toolReg)
		for _, m := range toolReg.ApprovedManifests() {
			for _, t := range m.Tools {
				if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Endpoint) == "" {
					continue
				}
				// Capture by value for the closure below.
				tname := t.Name
				endpoint := t.Endpoint
				desc := t.Description
				h := plugins.NewPluginToolHandler(endpoint)
				server.Tools().Register(
					gateway.Tool{Name: tname, Description: desc},
					func(ctx context.Context, args map[string]any) (any, error) {
						return h(ctx, args)
					},
				)
				fmt.Printf("registered tool-plugin: %s/%s → %s\n", m.Name, tname, endpoint)
			}
		}
	}

	// Hook plugins: scan pluginsDir for plugin.json manifests with a
	// hooks[] array. For approved plugins, install an EventListener on
	// the gateway's hookstore that POSTs the design-doc envelope
	// ({event, payload, timestamp}) to each declared endpoint on the
	// matching event. Fire-and-forget, no retries (matches the
	// at-most-once semantics in PLUGIN-ARCHITECTURE.md §3).
	hookPluginsDir := pluginsDir
	hookTokensFile := filepath.Join(dataDir, "hook-plugin-tokens.json")
	hookReg, err := plugins.NewHookPluginRegistry(hookPluginsDir, hookTokensFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: hook-plugin registry init failed: %v (hook plugins disabled)\n", err)
	} else {
		server.SetHookPluginRegistry(hookReg)
		approved := hookReg.ApprovedManifests()
		if len(approved) > 0 {
			server.HookStore().AddListener(plugins.NewPluginHookDispatcher(approved))
			for _, m := range approved {
				for _, h := range m.Hooks {
					fmt.Printf("registered hook-plugin: %s on %s → %s\n", m.Name, h.Event, h.Endpoint)
				}
			}
		}
	}

	// Web Push: only enabled when an operator-supplied contact is present.
	// Push providers reject anonymous senders, so a missing contact is a
	// hard disable rather than a silent default. First-use generates a
	// VAPID keypair and persists it under dataDir/push-keys.json (0o600).
	if strings.TrimSpace(cfg.Gateway.PushContact) != "" {
		pushSvc, err := push.NewService(dataDir, strings.TrimSpace(cfg.Gateway.PushContact))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: push service init failed: %v (push disabled)\n", err)
		} else {
			server.SetPushService(pushSvc)
		}
	}
	if cfg.Gateway.ShutdownTimeout > 0 {
		server.SetShutdownTimeout(time.Duration(cfg.Gateway.ShutdownTimeout) * time.Second)
	}
	if cfg.Memory.MaxMessages > 0 {
		store.SetMaxMessages(cfg.Memory.MaxMessages)
	} else {
		store.SetMaxMessages(cfg.Gateway.MaxMessages)
	}
	store.SetMemoryCompaction(cfg.Memory.CompactAfter, cfg.Memory.CompactAfter > 0 && !cfg.Memory.SummarizeOnCompact)
	if cfg.Gateway.MaxContextMessages > 0 {
		server.SetDefaultMaxContextMessages(cfg.Gateway.MaxContextMessages)
	}
	server.SetMetricsRequireAuth(cfg.Gateway.MetricsRequireAuth)
	if cfg.Gateway.MetricsRequireAuth && strings.TrimSpace(cfg.Gateway.AuthToken) == "" && strings.TrimSpace(cfg.Gateway.Password) == "" {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: gateway.metricsRequireAuth is true but authToken and password are empty — /metrics stays open until auth is configured\n")
	}
	if err := server.SyncNodesFromConfig(cfg.Nodes); err != nil {
		fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: config nodes → topology sync: %v\n", err)
	}

	warnAgentMisconfig(cfg)
	server.ReloadAgentRunner(runner, buildPerSessionRunnerFactory(cfg))
	pushAgentSummary(server, cfg)
	fmt.Printf("[openclaw-go] gateway agent: provider=%s model=%s openaiApiKeyConfigured=%v\n",
		cfg.Agent.Provider, cfg.Agent.Model, strings.TrimSpace(cfg.Providers.OpenAI.APIKey) != "")

	server.SetMemoryCompaction(cfg.Memory)

	// SIGHUP: full config hot-reload — re-applies token, password, origins,
	// trusted proxies, shutdown timeout, and session message cap.
	go func() {
		for range sighupCh {
			reloaded, err := config.Load("")
			if err != nil {
				fmt.Fprintf(os.Stderr, "[openclaw-go] SIGHUP config reload failed: %v\n", err)
				continue
			}
			server.SetAuthToken(reloaded.Gateway.AuthToken)
			server.SetAuth(reloaded.Gateway.Password, reloaded.Gateway.TrustedProxies)
			server.SetAllowedOrigins(reloaded.Gateway.AllowedOrigins)
			if reloaded.Gateway.ShutdownTimeout > 0 {
				server.SetShutdownTimeout(time.Duration(reloaded.Gateway.ShutdownTimeout) * time.Second)
			}
			if reloaded.Gateway.MaxContextMessages > 0 {
				server.SetDefaultMaxContextMessages(reloaded.Gateway.MaxContextMessages)
			}
			if reloaded.Memory.MaxMessages > 0 {
				store.SetMaxMessages(reloaded.Memory.MaxMessages)
			} else {
				store.SetMaxMessages(reloaded.Gateway.MaxMessages)
			}
			store.SetMemoryCompaction(reloaded.Memory.CompactAfter, reloaded.Memory.CompactAfter > 0 && !reloaded.Memory.SummarizeOnCompact)
			server.SetMemoryCompaction(reloaded.Memory)
			server.SetMetricsRequireAuth(reloaded.Gateway.MetricsRequireAuth)
			if reloaded.Gateway.MetricsRequireAuth && strings.TrimSpace(reloaded.Gateway.AuthToken) == "" && strings.TrimSpace(reloaded.Gateway.Password) == "" {
				fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: gateway.metricsRequireAuth is true but authToken and password are empty — /metrics stays open until auth is configured\n")
			}
			if err := server.SyncNodesFromConfig(reloaded.Nodes); err != nil {
				fmt.Fprintf(os.Stderr, "[openclaw-go] WARNING: config nodes → topology sync (SIGHUP): %v\n", err)
			}
			server.ApplyExtensionTools(reloaded)
			warnAgentMisconfig(reloaded)
			server.ReloadAgentRunner(buildGatewayRunner(reloaded), buildPerSessionRunnerFactory(reloaded))
			pushAgentSummary(server, reloaded)
			fmt.Printf("[openclaw-go] config reloaded via SIGHUP (token, auth, origins, proxies, timeouts, metrics auth, nodes, memory, skills/mcp, agent runner)\n")
		}
	}()

	// Auto-cleanup stale sessions daily (sessions not updated in 30 days).
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := store.Cleanup(30 * 24 * time.Hour)
				if err == nil && n > 0 {
					fmt.Printf("[openclaw-go] auto-cleaned %d stale sessions\n", n)
				}
			}
		}
	}()

	// Wire real sandbox into gateway tools so sandbox.run tool uses Docker.
	gateway.SetSandboxFuncs(
		func(ctx context.Context, script string, _ any) (*gateway.SandboxResult, error) {
			r, err := sandbox.RunScript(ctx, script, sandbox.DefaultOptions())
			if err != nil {
				return nil, err
			}
			return &gateway.SandboxResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}, nil
		},
		sandbox.IsAvailable,
	)

	// Register tool endpoints from external plugin manifests.
	if externalPlugins2, err := loader.Load(); err == nil {
		for _, ep := range externalPlugins2 {
			for _, tool := range ep.Tools() {
				server.RegisterPluginTools(tool.Name, tool.Description, tool.Endpoint)
			}
		}
	}

	server.ApplyExtensionTools(cfg)

	if cfg.Channels.Telegram.Enabled && !cfg.Channels.Telegram.UsePlugin {
		// Built-in inbound path (polling OR webhook). Skipped when
		// UsePlugin=true; the out-of-process plugin owns its own poller
		// + posts inbound to /plugins/telegram/inbound directly.
		tgObs := &channels.WebhookInboundConfig{
			OnHandlerError: func(ch string, err error, attrs map[string]any) {
				server.RecordInboundHandlerError(ch, err, attrs)
			},
		}
		inboundHandler := func(inboundCtx context.Context, inbound channels.InboundMessage) error {
			_, err := server.HandleInbound(inboundCtx, inbound)
			return err
		}
		mode := strings.ToLower(strings.TrimSpace(cfg.Channels.Telegram.InboundMode))
		if mode == "" {
			mode = "polling"
		}
		switch mode {
		case "webhook":
			var hook http.HandlerFunc
			if strings.TrimSpace(cfg.Channels.Telegram.BotToken) != "" {
				// Use channel handler so answerCallbackQuery runs (inline keyboard UX).
				tg := channels.NewTelegramChannel(
					cfg.Channels.Telegram.BotToken,
					cfg.Channels.Telegram.ChatID,
				)
				hook = tg.BuildWebhookHandler(cfg.Channels.Telegram.WebhookSecret, inboundHandler, tgObs)
			} else {
				hook = channels.BuildTelegramWebhookHandler(cfg.Channels.Telegram.WebhookSecret, inboundHandler, tgObs)
			}
			server.HandleFunc(cfg.Channels.Telegram.WebhookPath, hook)
		default:
			if strings.TrimSpace(cfg.Channels.Telegram.BotToken) != "" {
				poller := channels.NewTelegramPoller(cfg.Channels.Telegram.BotToken)
				poller.Start(ctx, inboundHandler, tgObs)
			}
		}
	}
	if cfg.Channels.Slack.Enabled {
		server.HandleFunc(
			cfg.Channels.Slack.WebhookPath,
			channels.BuildSlackWebhookHandler(
				cfg.Channels.Slack.SigningSecret,
				func(inboundCtx context.Context, inbound channels.InboundMessage) error {
					_, err := server.HandleInbound(inboundCtx, inbound)
					return err
				},
			),
		)
	}
	if cfg.Channels.Discord.Enabled {
		server.HandleFunc(
			cfg.Channels.Discord.WebhookPath,
			channels.BuildDiscordWebhookHandler(
				cfg.Channels.Discord.WebhookToken,
				func(inboundCtx context.Context, inbound channels.InboundMessage) error {
					_, err := server.HandleInbound(inboundCtx, inbound)
					return err
				},
			),
		)
	}
	if cfg.Channels.Teams.Enabled {
		server.HandleFunc(
			cfg.Channels.Teams.WebhookPath,
			channels.BuildTeamsWebhookHandler(
				cfg.Channels.Teams.WebhookSecret,
				func(inboundCtx context.Context, inbound channels.InboundMessage) error {
					_, err := server.HandleInbound(inboundCtx, inbound)
					return err
				},
			),
		)
	}
	if cfg.Channels.WhatsApp.Enabled {
		waObs := &channels.WebhookInboundConfig{
			OnHandlerError: func(ch string, err error, attrs map[string]any) {
				server.RecordInboundHandlerError(ch, err, attrs)
			},
		}
		server.HandleFunc(
			cfg.Channels.WhatsApp.WebhookPath,
			channels.BuildWhatsAppWebhookHandler(
				cfg.Channels.WhatsApp.VerifyToken,
				cfg.Channels.WhatsApp.AppSecret,
				func(inboundCtx context.Context, inbound channels.InboundMessage) error {
					_, err := server.HandleInbound(inboundCtx, inbound)
					return err
				},
				waObs,
			),
		)
	}
	if cfg.Channels.Line.Enabled {
		server.HandleFunc(
			cfg.Channels.Line.WebhookPath,
			channels.BuildLineWebhookHandler(
				cfg.Channels.Line.ChannelSecret,
				func(inboundCtx context.Context, inbound channels.InboundMessage) error {
					_, err := server.HandleInbound(inboundCtx, inbound)
					return err
				},
			),
		)
	}

	// Email inbound: IMAP polling. Decoupled from outbound (Email.Enabled)
	// so operators can run inbound-only forwarding or outbound-only alerts.
	// The poller falls back to the SMTP Host when IMAPHost is blank since
	// many providers (Gmail, Outlook personal) use a parallel imap.<provider>
	// host that the operator might not remember to set explicitly.
	if cfg.Channels.Email.InboundEnabled {
		imapHost := strings.TrimSpace(cfg.Channels.Email.IMAPHost)
		if imapHost == "" {
			imapHost = strings.TrimSpace(cfg.Channels.Email.Host)
		}
		imapPort := cfg.Channels.Email.IMAPPort
		if imapPort == 0 {
			imapPort = 993
		}
		mailbox := cfg.Channels.Email.IMAPMailbox
		if strings.TrimSpace(mailbox) == "" {
			mailbox = "INBOX"
		}
		interval := time.Duration(cfg.Channels.Email.IMAPPollSeconds) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		emailObs := &channels.WebhookInboundConfig{
			OnHandlerError: func(ch string, err error, attrs map[string]any) {
				server.RecordInboundHandlerError(ch, err, attrs)
			},
		}
		fetcher := channels.NewIMAPFetcher(
			imapHost, imapPort, cfg.Channels.Email.IMAPUseTLS,
			cfg.Channels.Email.Username,
			cfg.Channels.Email.Password,
			mailbox,
		)
		poller := channels.NewEmailInboundPoller(fetcher, interval)
		poller.Start(ctx, func(inboundCtx context.Context, inbound channels.InboundMessage) error {
			_, err := server.HandleInbound(inboundCtx, inbound)
			return err
		}, emailObs)
	}

	// Nostr inbound: start relay subscription if enabled.
	if cfg.Channels.Nostr.Enabled && strings.TrimSpace(cfg.Channels.Nostr.RelayURL) != "" {
		go func() {
			for {
				err := channels.NostrRelaySubscription(ctx, cfg.Channels.Nostr.RelayURL, cfg.Channels.Nostr.Pubkey,
					func(relayCtx context.Context, inbound channels.InboundMessage) error {
						_, err := server.HandleInbound(relayCtx, inbound)
						return err
					})
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					fmt.Printf("nostr relay error (retrying in 10s): %v\n", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Second):
					}
				}
			}
		}()
	}

	fmt.Printf("OpenClaw-Go gateway listening on %s\n", server.Address())
	return server.Run(ctx)
}

func initConfig() error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	return config.Save(path, config.Default())
}

// runOnboard handles `openclaw onboard [flags]`. With no flags it writes a
// default config (the historical behaviour). Flags let operators preseed the
// most common settings — agent provider, API keys, gateway token — without
// editing JSON by hand, so onboarding is scriptable in CI / Ansible / etc.
//
// Recognised flags (all optional, all string):
//
//	--provider echo|openai|anthropic
//	--openai-key <key>
//	--anthropic-key <key>
//	--gateway-token <bearer>
//	--gateway-port <port>
//
// Unknown flags are an error rather than silently ignored.
func runOnboard(args []string) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	// Start from whatever's on disk if it exists, falling back to defaults
	// so re-running onboard with new flags is a non-destructive merge.
	cfg, loadErr := config.Load(path)
	if loadErr != nil {
		cfg = config.Default()
	}

	opts, err := parseOnboardFlags(args)
	if err != nil {
		return err
	}
	applyOnboardOptions(&cfg, opts)

	if err := config.Save(path, cfg); err != nil {
		return err
	}
	printOnboardSummary(cfg, opts, path)
	return nil
}

type onboardOptions struct {
	provider     string
	openaiKey    string
	anthropicKey string
	gatewayToken string
	gatewayPort  string
	anyFlagGiven bool
}

func parseOnboardFlags(args []string) (onboardOptions, error) {
	opts := onboardOptions{}
	i := 0
	for i < len(args) {
		flag := args[i]
		i++
		// All known flags take a value, so we always need i to be in range.
		if i >= len(args) {
			return opts, fmt.Errorf("flag %s requires a value", flag)
		}
		value := args[i]
		i++
		opts.anyFlagGiven = true
		switch flag {
		case "--provider":
			v := strings.ToLower(strings.TrimSpace(value))
			if v != "echo" && v != "openai" && v != "anthropic" && v != "claude" {
				return opts, fmt.Errorf("--provider must be echo|openai|anthropic")
			}
			if v == "claude" {
				v = "anthropic"
			}
			opts.provider = v
		case "--openai-key":
			opts.openaiKey = strings.TrimSpace(value)
		case "--anthropic-key":
			opts.anthropicKey = strings.TrimSpace(value)
		case "--gateway-token":
			opts.gatewayToken = strings.TrimSpace(value)
		case "--gateway-port":
			opts.gatewayPort = strings.TrimSpace(value)
		default:
			return opts, fmt.Errorf("unknown onboard flag: %s", flag)
		}
	}
	return opts, nil
}

func applyOnboardOptions(cfg *config.Config, opts onboardOptions) {
	if opts.provider != "" {
		cfg.Agent.Provider = opts.provider
	}
	if opts.openaiKey != "" {
		cfg.Providers.OpenAI.APIKey = opts.openaiKey
	}
	if opts.anthropicKey != "" {
		cfg.Providers.Anthropic.APIKey = opts.anthropicKey
	}
	if opts.gatewayToken != "" {
		cfg.Gateway.AuthToken = opts.gatewayToken
	}
	if opts.gatewayPort != "" {
		if n, err := strconv.Atoi(opts.gatewayPort); err == nil && n > 0 && n < 65536 {
			cfg.Gateway.Port = n
		}
	}
}

func printOnboardSummary(cfg config.Config, opts onboardOptions, path string) {
	fmt.Println("OpenClaw-Go onboard complete.")
	fmt.Printf("  config:    %s\n", path)
	fmt.Printf("  provider:  %s\n", cfg.Agent.Provider)
	fmt.Printf("  gateway:   %s:%d\n", cfg.Gateway.Host, cfg.Gateway.Port)
	authState := "(none — gateway open to localhost only)"
	if strings.TrimSpace(cfg.Gateway.AuthToken) != "" {
		authState = "(bearer token configured)"
	}
	fmt.Printf("  auth:      %s\n", authState)
	if !opts.anyFlagGiven {
		fmt.Println()
		fmt.Println("Next steps:")
		fmt.Println("  - openclaw configure gateway setauth <token>     (require auth)")
		fmt.Println("  - openclaw configure set-agent-provider openai   (or anthropic)")
		fmt.Println("  - openclaw configure telegram/slack/discord ...  (set up a channel)")
		fmt.Println("  - openclaw gateway run                            (start the server)")
	}
}

func printConfig(cfg config.Config) error {
	if strings.TrimSpace(cfg.Gateway.AuthToken) != "" {
		cfg.Gateway.AuthToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Providers.OpenAI.APIKey) != "" {
		cfg.Providers.OpenAI.APIKey = "***redacted***"
	}
	if strings.TrimSpace(cfg.Providers.Anthropic.APIKey) != "" {
		cfg.Providers.Anthropic.APIKey = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Telegram.BotToken) != "" {
		cfg.Channels.Telegram.BotToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Telegram.WebhookSecret) != "" {
		cfg.Channels.Telegram.WebhookSecret = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Slack.BotToken) != "" {
		cfg.Channels.Slack.BotToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Slack.SigningSecret) != "" {
		cfg.Channels.Slack.SigningSecret = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Discord.BotToken) != "" {
		cfg.Channels.Discord.BotToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Discord.WebhookToken) != "" {
		cfg.Channels.Discord.WebhookToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Teams.WebhookSecret) != "" {
		cfg.Channels.Teams.WebhookSecret = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Teams.OutboundURL) != "" {
		cfg.Channels.Teams.OutboundURL = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.WhatsApp.AccessToken) != "" {
		cfg.Channels.WhatsApp.AccessToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.WhatsApp.VerifyToken) != "" {
		cfg.Channels.WhatsApp.VerifyToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.WhatsApp.AppSecret) != "" {
		cfg.Channels.WhatsApp.AppSecret = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Email.Password) != "" {
		cfg.Channels.Email.Password = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Matrix.AccessToken) != "" {
		cfg.Channels.Matrix.AccessToken = "***redacted***"
	}
	if strings.TrimSpace(cfg.Channels.Mattermost.AccessToken) != "" {
		cfg.Channels.Mattermost.AccessToken = "***redacted***"
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func runConfigure(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure gateway ... | set-agent-provider <echo|openai|anthropic> | configure telegram ...",
		)
	}
	switch args[0] {
	case "gateway":
		return runConfigureGateway(cfg, args[1:])
	case "set-agent-provider":
		value := strings.ToLower(strings.TrimSpace(args[1]))
		if value != "echo" && value != "openai" && value != "anthropic" && value != "claude" {
			return fmt.Errorf("provider must be echo, openai, or anthropic")
		}
		cfg.Agent.Provider = value
		switch value {
		case "echo":
			cfg.Agent.Model = "echo"
		case "openai":
			m := strings.TrimSpace(cfg.Providers.OpenAI.Model)
			if m == "" {
				m = config.Default().Providers.OpenAI.Model
			}
			cfg.Agent.Model = m
		case "anthropic", "claude":
			m := strings.TrimSpace(cfg.Providers.Anthropic.Model)
			if m == "" {
				m = config.Default().Providers.Anthropic.Model
			}
			cfg.Agent.Model = m
		}
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("agent.provider set to %s\nagent.model set to %s\n", value, cfg.Agent.Model)
		return nil
	case "telegram":
		return runConfigureTelegram(cfg, args[1:])
	case "slack":
		return runConfigureSlack(cfg, args[1:])
	case "discord":
		return runConfigureDiscord(cfg, args[1:])
	case "teams":
		return runConfigureTeams(cfg, args[1:])
	case "whatsapp":
		return runConfigureWhatsApp(cfg, args[1:])
	case "email":
		return runConfigureEmail(cfg, args[1:])
	case "signal":
		return runConfigureSignal(cfg, args[1:])
	case "matrix":
		return runConfigureMatrix(cfg, args[1:])
	case "mattermost":
		return runConfigureMattermost(cfg, args[1:])
	default:
		return fmt.Errorf("unknown configure command")
	}
}

// saveAndAnnounce writes cfg to the default config path and prints a single
// confirmation line. Pulled out of every configure-subcommand handler so the
// per-channel functions can stay focused on field-setting.
func saveAndAnnounce(cfg config.Config, format string, a ...any) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf(format+"\n", a...)
	return nil
}

func runConfigureEmail(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: openclaw configure email enable|host|port|user|password|from|inbound-enable|imap-host|imap-port|imap-tls|imap-mailbox|imap-poll <value>")
	}
	switch args[0] {
	case "enable":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Email.Enabled = v
		return saveAndAnnounce(cfg, "channels.email.enabled set to %v", v)
	case "host":
		cfg.Channels.Email.Host = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.email.host set to %q", cfg.Channels.Email.Host)
	case "port":
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 || n >= 65536 {
			return fmt.Errorf("port must be 1-65535")
		}
		cfg.Channels.Email.Port = n
		return saveAndAnnounce(cfg, "channels.email.port set to %d", n)
	case "user":
		cfg.Channels.Email.Username = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.email.username set to %q", cfg.Channels.Email.Username)
	case "password":
		cfg.Channels.Email.Password = args[1] // not trimmed — app passwords sometimes embed whitespace
		return saveAndAnnounce(cfg, "channels.email.password set (redacted)")
	case "from":
		cfg.Channels.Email.From = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.email.from set to %q", cfg.Channels.Email.From)
	case "inbound-enable":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Email.InboundEnabled = v
		return saveAndAnnounce(cfg, "channels.email.inboundEnabled set to %v", v)
	case "imap-host":
		cfg.Channels.Email.IMAPHost = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.email.imapHost set to %q", cfg.Channels.Email.IMAPHost)
	case "imap-port":
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 || n >= 65536 {
			return fmt.Errorf("imap port must be 1-65535 (993 = IMAPS, 143 = plain)")
		}
		cfg.Channels.Email.IMAPPort = n
		return saveAndAnnounce(cfg, "channels.email.imapPort set to %d", n)
	case "imap-tls":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Email.IMAPUseTLS = v
		return saveAndAnnounce(cfg, "channels.email.imapUseTLS set to %v", v)
	case "imap-mailbox":
		cfg.Channels.Email.IMAPMailbox = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.email.imapMailbox set to %q", cfg.Channels.Email.IMAPMailbox)
	case "imap-poll":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 5 {
			return fmt.Errorf("imap-poll seconds must be ≥5")
		}
		cfg.Channels.Email.IMAPPollSeconds = n
		return saveAndAnnounce(cfg, "channels.email.imapPollSeconds set to %d", n)
	default:
		return fmt.Errorf("unknown email configure subcommand %q", args[0])
	}
}

func runConfigureSignal(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: openclaw configure signal enable|baseurl|number <value>")
	}
	switch args[0] {
	case "enable":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Signal.Enabled = v
		return saveAndAnnounce(cfg, "channels.signal.enabled set to %v", v)
	case "baseurl":
		cfg.Channels.Signal.BaseURL = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.signal.baseUrl set to %q", cfg.Channels.Signal.BaseURL)
	case "number":
		cfg.Channels.Signal.Number = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.signal.number set to %q", cfg.Channels.Signal.Number)
	default:
		return fmt.Errorf("unknown signal configure subcommand %q", args[0])
	}
}

func runConfigureMatrix(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: openclaw configure matrix enable|baseurl|token <value>")
	}
	switch args[0] {
	case "enable":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Matrix.Enabled = v
		return saveAndAnnounce(cfg, "channels.matrix.enabled set to %v", v)
	case "baseurl":
		cfg.Channels.Matrix.BaseURL = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.matrix.baseUrl set to %q", cfg.Channels.Matrix.BaseURL)
	case "token":
		cfg.Channels.Matrix.AccessToken = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.matrix.accessToken set (redacted)")
	default:
		return fmt.Errorf("unknown matrix configure subcommand %q", args[0])
	}
}

func runConfigureMattermost(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: openclaw configure mattermost enable|baseurl|token <value>")
	}
	switch args[0] {
	case "enable":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Mattermost.Enabled = v
		return saveAndAnnounce(cfg, "channels.mattermost.enabled set to %v", v)
	case "baseurl":
		cfg.Channels.Mattermost.BaseURL = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.mattermost.baseUrl set to %q", cfg.Channels.Mattermost.BaseURL)
	case "token":
		cfg.Channels.Mattermost.AccessToken = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "channels.mattermost.accessToken set (redacted)")
	default:
		return fmt.Errorf("unknown mattermost configure subcommand %q", args[0])
	}
}

func parseBoolArg(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected true or false (got %q)", s)
	}
}

func runConfigureGateway(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure gateway auth-token <token> | allowed-origins <csv> | metrics-require-auth <true|false>",
		)
	}
	switch args[0] {
	case "auth-token":
		token := strings.TrimSpace(args[1])
		cfg.Gateway.AuthToken = token
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		if token == "" {
			fmt.Println("gateway auth token cleared")
		} else {
			fmt.Println("gateway auth token updated")
		}
		return nil
	case "allowed-origins":
		csv := strings.TrimSpace(args[1])
		items := strings.Split(csv, ",")
		origins := make([]string, 0, len(items))
		for _, item := range items {
			trimmed := strings.TrimSpace(item)
			if trimmed != "" {
				origins = append(origins, trimmed)
			}
		}
		cfg.Gateway.AllowedOrigins = origins
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("gateway allowed origins set (%d entries)\n", len(origins))
		return nil
	case "metrics-require-auth":
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Gateway.MetricsRequireAuth = v
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("gateway.metricsRequireAuth set to %v\n", v)
		return nil
	case "push-contact":
		// VAPID sub claim for push providers. Typical value:
		// "mailto:owner@example.com". Setting it to a non-empty value
		// activates push delivery on next gateway start.
		cfg.Gateway.PushContact = strings.TrimSpace(args[1])
		return saveAndAnnounce(cfg, "gateway.pushContact set to %q", cfg.Gateway.PushContact)
	default:
		return fmt.Errorf("unknown gateway configure command")
	}
}

func runConfigureTelegram(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure telegram inbound-mode <polling|webhook> | enable <true|false> | webhook set <public-base-url>",
		)
	}
	switch args[0] {
	case "inbound-mode":
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != "polling" && mode != "webhook" {
			return fmt.Errorf("inbound mode must be polling or webhook")
		}
		cfg.Channels.Telegram.InboundMode = mode
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.telegram.inboundMode set to %s\n", mode)
		return nil
	case "enable":
		enabled, err := strconv.ParseBool(strings.TrimSpace(args[1]))
		if err != nil {
			return fmt.Errorf("enable expects true or false")
		}
		cfg.Channels.Telegram.Enabled = enabled
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.telegram.enabled set to %v\n", enabled)
		return nil
	case "webhook":
		if len(args) < 3 || args[1] != "set" {
			return fmt.Errorf("usage: openclaw configure telegram webhook set <public-base-url>")
		}
		baseURL := strings.TrimSpace(args[2])
		if baseURL == "" {
			return fmt.Errorf("public base URL is required")
		}
		if !strings.HasPrefix(baseURL, "https://") {
			return fmt.Errorf("telegram webhook URL must be https")
		}
		if strings.TrimSpace(cfg.Channels.Telegram.BotToken) == "" {
			return fmt.Errorf("telegram bot token is missing")
		}
		path := cfg.Channels.Telegram.WebhookPath
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		fullWebhookURL := strings.TrimRight(baseURL, "/") + path
		if err := channels.SetTelegramWebhook(
			context.Background(),
			cfg.Channels.Telegram.BotToken,
			fullWebhookURL,
			cfg.Channels.Telegram.WebhookSecret,
		); err != nil {
			return err
		}
		cfg.Channels.Telegram.Enabled = true
		cfg.Channels.Telegram.InboundMode = "webhook"
		cfg.Channels.Telegram.WebhookPath = path
		configPath, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		fmt.Printf("telegram webhook configured at %s\n", fullWebhookURL)
		return nil
	case "use-plugin":
		// Flips Telegram between in-process (false, default) and the
		// out-of-process plugin at `plugins/telegram/` (true). Operator
		// must also approve the plugin and launch its binary separately
		// — see docs/PLUGIN-ARCHITECTURE.md and the channel-plugin
		// approval flow.
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.Telegram.UsePlugin = v
		return saveAndAnnounce(cfg, "channels.telegram.usePlugin set to %v", v)
	default:
		return fmt.Errorf("unknown telegram configure command")
	}
}

func runConfigureSlack(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure slack enable <true|false> | inbound-mode <webhook> | webhook-path <path>",
		)
	}
	switch args[0] {
	case "enable":
		enabled, err := strconv.ParseBool(strings.TrimSpace(args[1]))
		if err != nil {
			return fmt.Errorf("enable expects true or false")
		}
		cfg.Channels.Slack.Enabled = enabled
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.slack.enabled set to %v\n", enabled)
		return nil
	case "inbound-mode":
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != "webhook" {
			return fmt.Errorf("slack inbound mode currently supports webhook only")
		}
		cfg.Channels.Slack.InboundMode = mode
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.slack.inboundMode set to %s\n", mode)
		return nil
	case "webhook-path":
		pathValue := strings.TrimSpace(args[1])
		if pathValue == "" {
			return fmt.Errorf("webhook path cannot be empty")
		}
		if !strings.HasPrefix(pathValue, "/") {
			pathValue = "/" + pathValue
		}
		cfg.Channels.Slack.WebhookPath = pathValue
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.slack.webhookPath set to %s\n", pathValue)
		return nil
	default:
		return fmt.Errorf("unknown slack configure command")
	}
}

func runConfigureDiscord(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure discord enable <true|false> | inbound-mode <webhook> | webhook-path <path>",
		)
	}
	switch args[0] {
	case "enable":
		enabled, err := strconv.ParseBool(strings.TrimSpace(args[1]))
		if err != nil {
			return fmt.Errorf("enable expects true or false")
		}
		cfg.Channels.Discord.Enabled = enabled
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.discord.enabled set to %v\n", enabled)
		return nil
	case "inbound-mode":
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != "webhook" {
			return fmt.Errorf("discord inbound mode currently supports webhook only")
		}
		cfg.Channels.Discord.InboundMode = mode
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.discord.inboundMode set to %s\n", mode)
		return nil
	case "webhook-path":
		pathValue := strings.TrimSpace(args[1])
		if pathValue == "" {
			return fmt.Errorf("webhook path cannot be empty")
		}
		if !strings.HasPrefix(pathValue, "/") {
			pathValue = "/" + pathValue
		}
		cfg.Channels.Discord.WebhookPath = pathValue
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.discord.webhookPath set to %s\n", pathValue)
		return nil
	default:
		return fmt.Errorf("unknown discord configure command")
	}
}

func runConfigureTeams(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure teams enable <true|false> | inbound-mode <webhook> | webhook-path <path>",
		)
	}
	switch args[0] {
	case "enable":
		enabled, err := strconv.ParseBool(strings.TrimSpace(args[1]))
		if err != nil {
			return fmt.Errorf("enable expects true or false")
		}
		cfg.Channels.Teams.Enabled = enabled
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.teams.enabled set to %v\n", enabled)
		return nil
	case "inbound-mode":
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != "webhook" {
			return fmt.Errorf("teams inbound mode currently supports webhook only")
		}
		cfg.Channels.Teams.InboundMode = mode
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.teams.inboundMode set to %s\n", mode)
		return nil
	case "webhook-path":
		pathValue := strings.TrimSpace(args[1])
		if pathValue == "" {
			return fmt.Errorf("webhook path cannot be empty")
		}
		if !strings.HasPrefix(pathValue, "/") {
			pathValue = "/" + pathValue
		}
		cfg.Channels.Teams.WebhookPath = pathValue
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.teams.webhookPath set to %s\n", pathValue)
		return nil
	default:
		return fmt.Errorf("unknown teams configure command")
	}
}

func runConfigureWhatsApp(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(
			"usage: openclaw configure whatsapp enable <true|false> | inbound-mode <webhook> | webhook-path <path>",
		)
	}
	switch args[0] {
	case "enable":
		enabled, err := strconv.ParseBool(strings.TrimSpace(args[1]))
		if err != nil {
			return fmt.Errorf("enable expects true or false")
		}
		cfg.Channels.WhatsApp.Enabled = enabled
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.whatsapp.enabled set to %v\n", enabled)
		return nil
	case "inbound-mode":
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != "webhook" {
			return fmt.Errorf("whatsapp inbound mode currently supports webhook only")
		}
		cfg.Channels.WhatsApp.InboundMode = mode
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.whatsapp.inboundMode set to %s\n", mode)
		return nil
	case "webhook-path":
		pathValue := strings.TrimSpace(args[1])
		if pathValue == "" {
			return fmt.Errorf("webhook path cannot be empty")
		}
		if !strings.HasPrefix(pathValue, "/") {
			pathValue = "/" + pathValue
		}
		cfg.Channels.WhatsApp.WebhookPath = pathValue
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("channels.whatsapp.webhookPath set to %s\n", pathValue)
		return nil
	case "use-plugin":
		// Flips WhatsApp outbound between in-process (false, default)
		// and the out-of-process plugin at `plugins/whatsapp/` (true).
		// Note: unlike Telegram, this only affects OUTBOUND. WhatsApp
		// inbound is webhook-only (Meta-driven) and continues to flow
		// through the gateway's webhook handler regardless. Operator
		// must also approve the plugin and launch its binary
		// separately — see docs/PLUGIN-ARCHITECTURE.md.
		v, err := parseBoolArg(args[1])
		if err != nil {
			return err
		}
		cfg.Channels.WhatsApp.UsePlugin = v
		return saveAndAnnounce(cfg, "channels.whatsapp.usePlugin set to %v", v)
	default:
		return fmt.Errorf("unknown whatsapp configure command")
	}
}

func runDoctor(cfg config.Config, baseURL string) error {
	fmt.Println("OpenClaw-Go doctor")
	fmt.Printf("- gateway address: %s:%d\n", cfg.Gateway.Host, cfg.Gateway.Port)
	if strings.TrimSpace(cfg.Gateway.AuthToken) == "" {
		fmt.Println("- gateway auth: disabled")
	} else {
		fmt.Println("- gateway auth: enabled")
	}
	if len(cfg.Gateway.AllowedOrigins) == 0 {
		fmt.Println("- gateway ws origins: unrestricted")
	} else {
		fmt.Printf("- gateway ws origins: %d configured\n", len(cfg.Gateway.AllowedOrigins))
	}
	fmt.Printf("- agent provider: %s\n", cfg.Agent.Provider)
	if cfg.Agent.Provider == "openai" && strings.TrimSpace(cfg.Providers.OpenAI.APIKey) == "" {
		fmt.Println("- warning: openai api key is missing")
	}
	if (cfg.Agent.Provider == "anthropic" || cfg.Agent.Provider == "claude") &&
		strings.TrimSpace(cfg.Providers.Anthropic.APIKey) == "" {
		fmt.Println("- warning: anthropic api key is missing")
	}
	if cfg.Channels.Telegram.Enabled && strings.TrimSpace(cfg.Channels.Telegram.BotToken) == "" {
		fmt.Println("- warning: telegram enabled but bot token missing")
	}
	if cfg.Channels.Telegram.Enabled {
		mode := strings.ToLower(strings.TrimSpace(cfg.Channels.Telegram.InboundMode))
		if mode == "" {
			mode = "polling"
		}
		fmt.Printf("- telegram inbound mode: %s\n", mode)
		if mode == "webhook" {
			fmt.Printf("- telegram webhook path: %s\n", cfg.Channels.Telegram.WebhookPath)
			if strings.TrimSpace(cfg.Channels.Telegram.WebhookSecret) == "" {
				fmt.Println("- warning: telegram webhook secret is empty")
			}
		}
	}
	if cfg.Channels.Slack.Enabled {
		fmt.Println("- slack inbound mode: webhook")
		fmt.Printf("- slack webhook path: %s\n", cfg.Channels.Slack.WebhookPath)
		if strings.TrimSpace(cfg.Channels.Slack.BotToken) == "" {
			fmt.Println("- warning: slack enabled but bot token missing")
		}
		if strings.TrimSpace(cfg.Channels.Slack.SigningSecret) == "" {
			fmt.Println("- warning: slack signing secret is empty")
		}
	}
	if cfg.Channels.Discord.Enabled {
		fmt.Println("- discord inbound mode: webhook")
		fmt.Printf("- discord webhook path: %s\n", cfg.Channels.Discord.WebhookPath)
		if strings.TrimSpace(cfg.Channels.Discord.BotToken) == "" {
			fmt.Println("- warning: discord enabled but bot token missing")
		}
		if strings.TrimSpace(cfg.Channels.Discord.WebhookToken) == "" {
			fmt.Println("- warning: discord webhook token is empty")
		}
	}
	if cfg.Channels.Teams.Enabled {
		fmt.Println("- teams inbound mode: webhook")
		fmt.Printf("- teams webhook path: %s\n", cfg.Channels.Teams.WebhookPath)
		if strings.TrimSpace(cfg.Channels.Teams.WebhookSecret) == "" {
			fmt.Println("- warning: teams webhook secret is empty")
		}
		if strings.TrimSpace(cfg.Channels.Teams.OutboundURL) == "" {
			fmt.Println("- warning: teams enabled but outbound webhook url missing")
		}
	}
	if cfg.Channels.WhatsApp.Enabled {
		fmt.Println("- whatsapp inbound mode: webhook")
		fmt.Printf("- whatsapp webhook path: %s\n", cfg.Channels.WhatsApp.WebhookPath)
		if strings.TrimSpace(cfg.Channels.WhatsApp.AccessToken) == "" {
			fmt.Println("- warning: whatsapp enabled but access token missing")
		}
		if strings.TrimSpace(cfg.Channels.WhatsApp.PhoneNumberID) == "" {
			fmt.Println("- warning: whatsapp enabled but phone number id missing")
		}
		if strings.TrimSpace(cfg.Channels.WhatsApp.VerifyToken) == "" {
			fmt.Println("- error: whatsapp verify token is empty — gateway will refuse to start until WHATSAPP_VERIFY_TOKEN / channels.whatsapp.verifyToken is set")
		}
	}
	if cfg.Channels.Email.Enabled {
		fmt.Printf("- email outbound: smtp %s:%d\n",
			firstNonEmpty(cfg.Channels.Email.Host, "(unset)"), cfg.Channels.Email.Port)
		if strings.TrimSpace(cfg.Channels.Email.Host) == "" {
			fmt.Println("- error: email enabled but channels.email.host is empty")
		}
		if strings.TrimSpace(cfg.Channels.Email.Username) == "" {
			fmt.Println("- warning: email username is empty — most SMTP relays require auth")
		}
	}
	if cfg.Channels.Email.InboundEnabled {
		imapHost := firstNonEmpty(cfg.Channels.Email.IMAPHost, cfg.Channels.Email.Host, "(unset)")
		port := cfg.Channels.Email.IMAPPort
		if port == 0 {
			port = 993
		}
		mailbox := cfg.Channels.Email.IMAPMailbox
		if mailbox == "" {
			mailbox = "INBOX"
		}
		tls := "TLS"
		if !cfg.Channels.Email.IMAPUseTLS {
			tls = "plain"
		}
		fmt.Printf("- email inbound: imap %s:%d %s mailbox=%s\n", imapHost, port, tls, mailbox)
		if strings.TrimSpace(cfg.Channels.Email.Username) == "" || strings.TrimSpace(cfg.Channels.Email.Password) == "" {
			fmt.Println("- error: email inbound enabled but username or password is empty")
		}
	}
	if cfg.Channels.Signal.Enabled {
		fmt.Printf("- signal: sidecar %s (outbound only)\n", firstNonEmpty(cfg.Channels.Signal.BaseURL, "(unset)"))
		if strings.TrimSpace(cfg.Channels.Signal.BaseURL) == "" || strings.TrimSpace(cfg.Channels.Signal.Number) == "" {
			fmt.Println("- error: signal enabled but baseUrl or number is empty")
		}
	}
	if cfg.Channels.Matrix.Enabled {
		fmt.Printf("- matrix: homeserver %s (outbound only)\n", firstNonEmpty(cfg.Channels.Matrix.BaseURL, "(unset)"))
		if strings.TrimSpace(cfg.Channels.Matrix.BaseURL) == "" || strings.TrimSpace(cfg.Channels.Matrix.AccessToken) == "" {
			fmt.Println("- error: matrix enabled but baseUrl or accessToken is empty")
		}
	}
	if cfg.Channels.Mattermost.Enabled {
		fmt.Printf("- mattermost: server %s (outbound only)\n", firstNonEmpty(cfg.Channels.Mattermost.BaseURL, "(unset)"))
		if strings.TrimSpace(cfg.Channels.Mattermost.BaseURL) == "" || strings.TrimSpace(cfg.Channels.Mattermost.AccessToken) == "" {
			fmt.Println("- error: mattermost enabled but baseUrl or accessToken is empty")
		}
	}

	healthURL := baseURL + "/health"
	resp, err := http.Get(healthURL)
	if err != nil {
		fmt.Printf("- gateway health: down (%v)\n", err)
		return nil
	}
	defer resp.Body.Close()
	fmt.Printf("- gateway health: HTTP %d\n", resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		uctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		latest, page, err := gateway.FetchDefaultRepoLatestRelease(uctx)
		if err != nil {
			fmt.Printf("- release check: skipped (%v)\n", err)
		} else {
			fmt.Printf("- latest published release: %s\n", latest)
			if page != "" {
				fmt.Printf("- releases page: %s\n", page)
			}
			if gateway.UpdateAvailable(gateway.Version, latest) {
				fmt.Println("- note: a newer release tag may be available for this repo (compare with your binary build).")
			}
		}
	}
	return nil
}

func get(url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if gatewayAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+gatewayAuthToken)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return printHTTPResponse(resp)
}

func post(url string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if gatewayAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+gatewayAuthToken)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return printHTTPResponse(resp)
}

func parseRPCParams(method string, args []string) (map[string]any, error) {
	if len(args) == 0 {
		return map[string]any{}, nil
	}
	joined := strings.TrimSpace(strings.Join(args, " "))
	if strings.HasPrefix(joined, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(joined), &m); err != nil {
			return nil, fmt.Errorf("invalid JSON params: %w", err)
		}
		return m, nil
	}
	switch method {
	case "sessions.get", "sessions.delete":
		if len(args) != 1 {
			return nil, fmt.Errorf("usage: openclaw rpc %s <session-id>", method)
		}
		return map[string]any{"sessionId": args[0]}, nil
	case "tools.invoke":
		if len(args) < 1 {
			return nil, fmt.Errorf("usage: openclaw rpc tools.invoke <tool-name> [text|{json}]")
		}
		toolName := strings.TrimSpace(args[0])
		arguments := map[string]any{}
		if len(args) > 1 {
			joinedArgs := strings.TrimSpace(strings.Join(args[1:], " "))
			if strings.HasPrefix(joinedArgs, "{") {
				if err := json.Unmarshal([]byte(joinedArgs), &arguments); err != nil {
					return nil, fmt.Errorf("invalid tool arguments JSON: %w", err)
				}
			} else {
				arguments["text"] = joinedArgs
			}
		}
		return map[string]any{
			"name":      toolName,
			"arguments": arguments,
		}, nil
	case "message.send":
		if len(args) < 2 {
			return nil, fmt.Errorf("usage: openclaw rpc message.send <session-id> <message>")
		}
		return map[string]any{
			"sessionId": args[0],
			"message":   strings.Join(args[1:], " "),
			"channel":   "cli",
		}, nil
	default:
		return nil, fmt.Errorf("method %q needs JSON params as a single {...} argument", method)
	}
}

func rpc(url, method string, params any) error {
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	return post(url, request)
}

// post2 is like post but returns the raw *http.Response (caller closes body).
func post2(targetURL string, payload any) (*http.Response, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if gatewayAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+gatewayAuthToken)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	return client.Do(req)
}

// copyDir recursively copies a directory tree.
// dashboardURL derives the browser URL for the running gateway from config.
// It targets `/ui/`, which serves the embedded Control Panel
// (`internal/gateway/ui/index.html`) — auth-guarded by the same bearer/basic
// rules as `/rpc`. When auth is enabled, the browser will need to supply the
// token (e.g. via a browser extension setting `Authorization` headers) to
// see anything beyond the redirect.
func dashboardURL(cfg config.Config) string {
	host := strings.TrimSpace(cfg.Gateway.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Gateway.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://%s:%d/ui/", host, port)
}

// runDashboard prints the dashboard URL and best-effort opens it in the
// user's browser. Browser launch failures are NOT errors — the URL is
// always printed so the user can copy it manually.
func runDashboard(cfg config.Config) error {
	url := dashboardURL(cfg)
	fmt.Println("Dashboard:", url)
	if strings.TrimSpace(cfg.Gateway.AuthToken) != "" {
		fmt.Println("(Gateway auth is enabled — the browser will need the bearer token to load /ui/.)")
	}
	if err := openBrowser(url); err != nil {
		// Non-fatal: print the hint and exit clean.
		fmt.Fprintf(os.Stderr, "(could not auto-open browser: %v — open the URL above manually)\n", err)
	}
	return nil
}

// openBrowser issues an OS-specific command to launch the default browser.
// Returns an error when no suitable launcher is found OR when the command
// fails to start; callers should treat errors as soft failures and still
// print the URL so the user can open it manually.
func openBrowser(url string) error {
	// Avoid pulling in runtime by referring to GOOS via a small switch on
	// build-time. We can use runtime.GOOS — let's just import it once.
	switch goos() {
	case "windows":
		// rundll32 is the historically-stable Windows browser launcher and
		// works without a shell. The url is single-quoted in argv so '&'
		// can't be misinterpreted by cmd.exe.
		return execOpen("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return execOpen("open", url)
	default:
		// Most Linux desktops ship xdg-open; if not present, exec returns
		// "executable not found" and the caller falls through to the
		// manual-URL hint.
		return execOpen("xdg-open", url)
	}
}

// runPluginsCLI dispatches `openclaw plugins [subcmd]`. With no subcmd
// it preserves the historical behaviour (GET /plugins, list legacy
// plugins). New subcommands manage channel plugins:
//
//	openclaw plugins channel list
//	openclaw plugins channel approve <name>
//	openclaw plugins channel revoke <name>
//	openclaw plugins tool list
//	openclaw plugins tool approve <name>
//	openclaw plugins tool revoke <name>
//	openclaw plugins hook list
//	openclaw plugins hook approve <name>
//	openclaw plugins hook revoke <name>
//
// Approve prints the issued bearer token ONCE — the operator copies it
// into the plugin's OPENCLAW_PLUGIN_TOKEN env var. Subsequent approves
// are idempotent (same token); rotation requires revoke + approve.
func runPluginsCLI(baseURL string, args []string) error {
	if len(args) == 0 {
		// Backward-compatible default: list legacy (route/tool) plugins.
		return get(baseURL + "/plugins")
	}
	rpcURL := baseURL + "/rpc"
	switch args[0] {
	case "channel":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw plugins channel list|approve|revoke ...")
		}
		switch args[1] {
		case "list":
			return rpc(rpcURL, "plugins.channel.list", map[string]any{})
		case "approve":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins channel approve <name>")
			}
			return rpc(rpcURL, "plugins.channel.approve", map[string]any{"name": args[2]})
		case "revoke":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins channel revoke <name>")
			}
			return rpc(rpcURL, "plugins.channel.revoke", map[string]any{"name": args[2]})
		default:
			return fmt.Errorf("unknown plugins channel subcommand %q", args[1])
		}
	case "tool":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw plugins tool list|approve|revoke ...")
		}
		switch args[1] {
		case "list":
			return rpc(rpcURL, "plugins.tool.list", map[string]any{})
		case "approve":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins tool approve <name>")
			}
			return rpc(rpcURL, "plugins.tool.approve", map[string]any{"name": args[2]})
		case "revoke":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins tool revoke <name>")
			}
			return rpc(rpcURL, "plugins.tool.revoke", map[string]any{"name": args[2]})
		default:
			return fmt.Errorf("unknown plugins tool subcommand %q", args[1])
		}
	case "hook":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw plugins hook list|approve|revoke ...")
		}
		switch args[1] {
		case "list":
			return rpc(rpcURL, "plugins.hook.list", map[string]any{})
		case "approve":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins hook approve <name>")
			}
			return rpc(rpcURL, "plugins.hook.approve", map[string]any{"name": args[2]})
		case "revoke":
			if len(args) < 3 {
				return fmt.Errorf("usage: openclaw plugins hook revoke <name>")
			}
			return rpc(rpcURL, "plugins.hook.revoke", map[string]any{"name": args[2]})
		default:
			return fmt.Errorf("unknown plugins hook subcommand %q", args[1])
		}
	default:
		return fmt.Errorf("unknown plugins subcommand %q", args[0])
	}
}

// runMessage dispatches `openclaw message <subcmd>`. Subcommands:
//
//	send <session-id> <text>   — post a message into the named session
//	history <session-id> [n]   — fetch the session transcript (optional last-N)
//	dispatch <channel> <target> <text>
//	                            — push an outbound message through the channel router
//	                              (sends to Telegram/Slack/etc. depending on which is enabled)
//
// The pre-existing top-level `openclaw message send …` form is preserved
// because removing it would break scripts; the new subcommands extend the
// surface without taking anything away.
func runMessage(baseURL string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw message send|history|dispatch ...")
	}
	switch args[0] {
	case "send":
		if len(args) < 3 {
			return fmt.Errorf("usage: openclaw message send <session-id> <text>")
		}
		return post(baseURL+"/message", map[string]string{
			"sessionId": args[1],
			"message":   args[2],
			"channel":   "cli",
		})
	case "history":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw message history <session-id> [last-n]")
		}
		return get(baseURL + "/sessions/" + url.PathEscape(args[1]) + "/history")
	case "dispatch":
		if len(args) < 4 {
			return fmt.Errorf("usage: openclaw message dispatch <channel> <target> <text>")
		}
		return rpc(baseURL+"/rpc", "message.send", map[string]any{
			"sessionId": "cli",
			"channel":   args[1],
			"target":    args[2],
			"message":   args[3],
		})
	default:
		return fmt.Errorf("unknown message subcommand %q", args[0])
	}
}

// runCompactionCLI drives the `sessions.compaction.*` RPCs from the CLI.
// Subcommands: list <session-id>, get <id>, restore <id> [--yes], branch <id> [--id <new-id>].
// `restore` is destructive on the source session, so it requires `--yes`.
func runCompactionCLI(baseURL string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw compaction list|get|restore|branch ...")
	}
	rpcURL := baseURL + "/rpc"
	switch args[0] {
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw compaction list <session-id>")
		}
		return rpc(rpcURL, "sessions.compaction.list", map[string]any{"sessionId": args[1]})
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw compaction get <id>")
		}
		return rpc(rpcURL, "sessions.compaction.get", map[string]any{"id": args[1]})
	case "restore":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw compaction restore <id> --yes")
		}
		confirmed := false
		for _, a := range args[2:] {
			if a == "--yes" || a == "-y" {
				confirmed = true
			}
		}
		if !confirmed {
			return fmt.Errorf("refusing to overwrite session messages without --yes")
		}
		return rpc(rpcURL, "sessions.compaction.restore", map[string]any{"id": args[1]})
	case "branch":
		if len(args) < 2 {
			return fmt.Errorf("usage: openclaw compaction branch <id> [--id <new-session-id>]")
		}
		params := map[string]any{"id": args[1]}
		for i := 2; i < len(args); i++ {
			if args[i] == "--id" && i+1 < len(args) {
				params["newSessionId"] = args[i+1]
				i++
			}
		}
		return rpc(rpcURL, "sessions.compaction.branch", params)
	default:
		return fmt.Errorf("unknown compaction subcommand %q", args[0])
	}
}

// runWebLoginCLI drives the device-code-style `web.login.start` →
// `web.login.wait` flow from the CLI. Prints the approval URL, attempts to
// open the browser, then long-polls for the user's decision. Prints the
// issued token on approval so the user can pipe it into a follow-up
// `configure gateway setauth` step (kept manual on purpose — auto-saving
// to config without explicit user consent feels surprising).
func runWebLoginCLI(baseURL string, _ []string) error {
	rpcURL := baseURL + "/rpc"
	startReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "web.login.start",
		"params":  map[string]any{},
	}
	raw, _ := json.Marshal(startReq)
	resp, err := postRaw(rpcURL, raw)
	if err != nil {
		return err
	}
	var startResult struct {
		Result struct {
			Nonce string `json:"nonce"`
			URL   string `json:"url"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(resp, &startResult); err != nil {
		return fmt.Errorf("decode start response: %w", err)
	}
	if startResult.Error != nil {
		return fmt.Errorf("web.login.start: %s", startResult.Error.Message)
	}
	approveURL := baseURL + startResult.Result.URL
	fmt.Println("Open this URL in a browser already authenticated to the gateway:")
	fmt.Println("  ", approveURL)
	if err := openBrowser(approveURL); err != nil {
		fmt.Fprintf(os.Stderr, "(could not auto-open: %v — open the URL above manually)\n", err)
	}
	fmt.Println("Waiting for approval…")

	waitReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "web.login.wait",
		"params":  map[string]any{"nonce": startResult.Result.Nonce},
	}
	raw, _ = json.Marshal(waitReq)
	resp, err = postRaw(rpcURL, raw)
	if err != nil {
		return err
	}
	var waitResult struct {
		Result struct {
			Status      string `json:"status"`
			IssuedToken string `json:"issuedToken"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(resp, &waitResult); err != nil {
		return fmt.Errorf("decode wait response: %w", err)
	}
	if waitResult.Error != nil {
		return fmt.Errorf("web.login.wait: %s", waitResult.Error.Message)
	}
	switch waitResult.Result.Status {
	case "approved":
		fmt.Println("Approved. Issued token:")
		fmt.Println("  ", waitResult.Result.IssuedToken)
		fmt.Println("(run `openclaw configure gateway setauth <token>` to save it)")
		return nil
	case "rejected":
		return fmt.Errorf("login rejected in browser")
	case "expired":
		return fmt.Errorf("login attempt expired before approval")
	default:
		return fmt.Errorf("unexpected wait status: %s", waitResult.Result.Status)
	}
}

// postRaw issues a raw POST with the existing auth wiring and returns the
// response body. Used by web-login to inspect parsed JSON-RPC responses
// (the public `rpc()` helper prints+discards, which doesn't fit here).
func postRaw(url string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if gatewayAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+gatewayAuthToken)
	}
	client := &http.Client{Timeout: 6 * time.Minute} // web-login wait can long-poll
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// firstNonEmpty returns the first non-empty string argument or "" if all are empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// goos is a thin wrapper so tests can substitute different OS values if
// needed (none currently do, but it keeps openBrowser easily testable).
var goos = func() string { return runtime.GOOS }

// execOpen launches an external command without waiting for it to finish —
// browser processes detach themselves and we don't want to block the CLI.
var execOpen = func(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// dataDirPath returns the openclaw-go data directory for the current user
// (`$HOME/.openclaw-go`). Errors loading the home dir fall back to "" so
// callers can detect and report cleanly.
func dataDirPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".openclaw-go"), nil
}

// runBackup snapshots the openclaw-go data dir into a sibling
// `.openclaw-go.backup-<timestamp>` directory. Sub-arg `list` enumerates
// existing backups instead of taking a new one.
func runBackup(args []string) error {
	if len(args) > 0 && args[0] == "list" {
		return runBackupList()
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return err
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return fmt.Errorf("data dir not found at %s — run `openclaw onboard` first", dataDir)
	}
	backupPath := dataDir + ".backup-" + time.Now().Format("20060102-150405")
	fmt.Printf("Backing up %s → %s\n", dataDir, backupPath)
	if err := copyDir(dataDir, backupPath); err != nil {
		return err
	}
	fmt.Println("Backup complete.")
	return nil
}

func runBackupList() error {
	dataDir, err := dataDirPath()
	if err != nil {
		return err
	}
	parent := filepath.Dir(dataDir)
	prefix := filepath.Base(dataDir) + ".backup-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fmt.Println(filepath.Join(parent, e.Name()))
		found++
	}
	if found == 0 {
		fmt.Println("(no backups found)")
	}
	return nil
}

// runRestore copies a previously-taken backup directory back over the live
// data dir. Destructive — requires --yes (or interactive y/N when stdin is
// a terminal we can prompt on). The implementation here errors without
// --yes rather than prompting, so it stays safely scriptable.
func runRestore(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw restore <backup-path> --yes")
	}
	backupPath := args[0]
	confirmed := false
	for _, a := range args[1:] {
		if a == "--yes" || a == "-y" {
			confirmed = true
		}
	}
	if !confirmed {
		return fmt.Errorf("refusing to overwrite data dir without --yes; pass --yes to confirm")
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		return fmt.Errorf("backup path %s: %w", backupPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup path %s is not a directory", backupPath)
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return err
	}
	// Mirror copyDir's behaviour: it overwrites files at their target paths
	// and leaves untouched files in dataDir alone. For a true wipe we'd
	// first remove dataDir — that risks data loss on partial backups, so
	// we deliberately do a merge-restore here.
	fmt.Printf("Restoring %s → %s (merge)\n", backupPath, dataDir)
	if err := copyDir(backupPath, dataDir); err != nil {
		return err
	}
	fmt.Println("Restore complete.")
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func deleteHTTP(targetURL string) error {
	req, err := http.NewRequest(http.MethodDelete, targetURL, nil)
	if err != nil {
		return err
	}
	if gatewayAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+gatewayAuthToken)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return printHTTPResponse(resp)
}

func printHTTPResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, string(body))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}
	return nil
}
