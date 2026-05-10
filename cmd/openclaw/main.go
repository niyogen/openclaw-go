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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/config"
	"openclaw-go/internal/gateway"
	"openclaw-go/internal/plugins"
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
		if err := initConfig(); err != nil {
			fmt.Fprintf(os.Stderr, "onboard error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("OpenClaw-Go onboard complete.")
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
		if len(os.Args) < 5 || os.Args[2] != "send" {
			fmt.Println("usage: openclaw message send <session-id> <message>")
			os.Exit(2)
		}
		payload := map[string]string{
			"sessionId": os.Args[3],
			"message":   os.Args[4],
			"channel":   "cli",
		}
		if err := post(baseURL+"/message", payload); err != nil {
			fmt.Fprintf(os.Stderr, "message send error: %v\n", err)
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
		if err := get(baseURL + "/plugins"); err != nil {
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
		home, _ := os.UserHomeDir()
		dataDir := filepath.Join(home, ".openclaw-go")
		backupPath := dataDir + ".backup-" + time.Now().Format("20060102-150405")
		fmt.Printf("Backing up %s → %s\n", dataDir, backupPath)
		if err := copyDir(dataDir, backupPath); err != nil {
			fmt.Fprintf(os.Stderr, "backup error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Backup complete.")
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
			"model": "text-embedding-ada-002",
			"input": texts,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "embeddings error: %v\n", err)
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
	fmt.Println("  openclaw onboard")
	fmt.Println("  openclaw config init|show")
	fmt.Println("  openclaw configure gateway auth-token <token>")
	fmt.Println("  openclaw configure gateway allowed-origins <csv>")
	fmt.Println("  openclaw configure gateway metrics-require-auth <true|false>")
	fmt.Println("  openclaw configure set-agent-provider <echo|openai|anthropic>")
	fmt.Println("  openclaw configure telegram inbound-mode <polling|webhook>")
	fmt.Println("  openclaw configure telegram enable <true|false>")
	fmt.Println("  openclaw configure telegram webhook set <public-base-url>")
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
	fmt.Println("  openclaw logs [<level>]")
	fmt.Println("  openclaw cron [list|add <id> <schedule> <cmd>|delete <id>]")
	fmt.Println("  openclaw hooks [list|add <id> <event> <type> <target>|delete <id>]")
	fmt.Println("  openclaw secrets [list|set <name> <value>|delete <name>]")
	fmt.Println("  openclaw plugins")
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
	fmt.Println("  openclaw usage")
	fmt.Println("  openclaw channels")
	fmt.Println("  openclaw nodes")
	fmt.Println("  openclaw skills")
	fmt.Println("  openclaw mcp")
	fmt.Println("  openclaw memory")
	fmt.Println("  openclaw rpc <method> [args...]")
	fmt.Println("  openclaw sessions")
	fmt.Println("  openclaw session get|history|kill|delete|patch|compact|stats <id>")
	fmt.Println("  openclaw message send <session-id> <message>")
	fmt.Println("  openclaw agent <message>")
}

func runGateway(cfg config.Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	statePath := filepath.Join(home, ".openclaw-go", "sessions.json")
	store, err := sessions.New(statePath)
	if err != nil {
		return err
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
	channelRouter := channels.NewRouter()
	if cfg.Channels.Webhook.Enabled {
		channelRouter.Register(channels.NewWebhookChannel(cfg.Channels.Webhook.OutboundURL))
	}
	if cfg.Channels.Telegram.Enabled {
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
	if cfg.Channels.WhatsApp.Enabled {
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
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))

	// Load external plugins from configured directory.
	pluginsDir := cfg.Gateway.PluginsDir
	if pluginsDir == "" {
		pluginsDir = filepath.Join(home, ".openclaw-go", "plugins")
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
		filepath.Join(home, ".openclaw-go"),
	)

	// Configure additional auth modes (password + trusted proxies).
	server.SetAuth(cfg.Gateway.Password, cfg.Gateway.TrustedProxies)
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

	// RunnerFactory enables per-session model routing.
	server.SetRunnerFactory(func(provider, model string) agents.Runner {
		return agents.NewRunnerFromOptions(agents.RunnerOptions{
			Provider:         provider,
			OpenAIAPIKey:     cfg.Providers.OpenAI.APIKey,
			OpenAIBaseURL:    cfg.Providers.OpenAI.BaseURL,
			OpenAIModel:      model,
			AnthropicAPIKey:  cfg.Providers.Anthropic.APIKey,
			AnthropicBaseURL: cfg.Providers.Anthropic.BaseURL,
			AnthropicModel:   model,
		})
	})

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
			fmt.Printf("[openclaw-go] config reloaded via SIGHUP (token, auth, origins, proxies, timeouts, metrics auth, nodes, memory, skills/mcp)\n")
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
		func(ctx context.Context, script string, _ interface{}) (*gateway.SandboxResult, error) {
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

	if cfg.Channels.WhatsApp.Enabled && strings.TrimSpace(cfg.Channels.WhatsApp.VerifyToken) == "" {
		return fmt.Errorf("whatsapp is enabled but verify token is empty: set channels.whatsapp.verifyToken or WHATSAPP_VERIFY_TOKEN")
	}

	if cfg.Channels.Telegram.Enabled {
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
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Printf("agent.provider set to %s\n", value)
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
	default:
		return fmt.Errorf("unknown configure command")
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
