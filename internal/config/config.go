package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"openclaw-go/internal/fileutil"
)

type GatewayConfig struct {
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	AuthToken          string   `json:"authToken"`
	Password           string   `json:"password"` // HTTP Basic password (alternative to token)
	AllowedOrigins     []string `json:"allowedOrigins"`
	PluginsDir         string   `json:"pluginsDir"`
	TrustedProxies     []string `json:"trustedProxies"`     // IPs/CIDRs that may set X-Forwarded-For
	ShutdownTimeout    int      `json:"shutdownTimeout"`    // graceful drain in seconds (default 5)
	MaxMessages        int      `json:"maxMessages"`        // per-session message cap (0 = unlimited)
	MaxContextMessages int      `json:"maxContextMessages"` // context window truncation (0 = unlimited)
	// MetricsRequireAuth when true makes GET /metrics use the same auth rules as
	// other gateway routes (Bearer token, X-OpenClaw-Token, ?token=, Basic password,
	// or trusted proxy). When false, /metrics is public for scrapers. If no gateway
	// auth is configured, this flag has no practical effect (requests stay authorized).
	MetricsRequireAuth bool `json:"metricsRequireAuth"`
	// PushContact is the RFC 8292 `sub` claim presented in VAPID JWTs —
	// typically `mailto:owner@example.com`. Push providers (FCM, Mozilla,
	// etc.) reject anonymous senders, so leaving this blank disables push.
	PushContact string `json:"pushContact"`
}

type AgentConfig struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type OpenAIConfig struct {
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
}

type AnthropicConfig struct {
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
}

type ProvidersConfig struct {
	OpenAI    OpenAIConfig    `json:"openai"`
	Anthropic AnthropicConfig `json:"anthropic"`
}

type WebhookChannelConfig struct {
	Enabled     bool   `json:"enabled"`
	OutboundURL string `json:"outboundUrl"`
}

type TelegramChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"botToken"`
	ChatID        string `json:"chatId"`
	InboundMode   string `json:"inboundMode"`
	WebhookPath   string `json:"webhookPath"`
	WebhookSecret string `json:"webhookSecret"`
}

type SlackChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"botToken"`
	ChannelID     string `json:"channelId"`
	InboundMode   string `json:"inboundMode"`
	WebhookPath   string `json:"webhookPath"`
	SigningSecret string `json:"signingSecret"`
}

type DiscordChannelConfig struct {
	Enabled      bool   `json:"enabled"`
	BotToken     string `json:"botToken"`
	ChannelID    string `json:"channelId"`
	InboundMode  string `json:"inboundMode"`
	WebhookPath  string `json:"webhookPath"`
	WebhookToken string `json:"webhookToken"`
}

type TeamsChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	OutboundURL   string `json:"outboundUrl"`
	InboundMode   string `json:"inboundMode"`
	WebhookPath   string `json:"webhookPath"`
	WebhookSecret string `json:"webhookSecret"`
}

type WhatsAppChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	AccessToken   string `json:"accessToken"`
	PhoneNumberID string `json:"phoneNumberId"`
	ToNumber      string `json:"toNumber"`
	InboundMode   string `json:"inboundMode"`
	WebhookPath   string `json:"webhookPath"`
	VerifyToken   string `json:"verifyToken"`
	AppSecret     string `json:"appSecret"`
}

type LineChannelConfig struct {
	Enabled       bool   `json:"enabled"`
	ChannelToken  string `json:"channelToken"`
	ChannelSecret string `json:"channelSecret"`
	WebhookPath   string `json:"webhookPath"`
}

type NostrChannelConfig struct {
	Enabled  bool   `json:"enabled"`
	RelayURL string `json:"relayUrl"`
	Pubkey   string `json:"pubkey"`
}

// EmailChannelConfig configures the SMTP-outbound email channel and (when
// InboundEnabled is true) the IMAP-inbound poller.
//
// Outbound: Port 465 = implicit TLS, 587 = STARTTLS. Empty Host disables.
//
// Inbound: connects to IMAPHost:IMAPPort using the same Username/Password
// as outbound (most providers issue a single credential for both). Polls
// IMAPMailbox (default "INBOX") every IMAPPollSeconds seconds for unseen
// messages and dispatches each to the agent. Set InboundEnabled=false to
// run outbound-only.
type EmailChannelConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`     // e.g. "smtp.gmail.com"
	Port     int    `json:"port"`     // 587 (STARTTLS, default) or 465 (TLS)
	Username string `json:"username"` // SMTP+IMAP AUTH user (usually the From address)
	Password string `json:"password"` // app-password preferred over account password
	From     string `json:"from"`     // RFC 5322 From; defaults to Username when empty

	// Inbound (IMAP) — optional. Outbound and inbound can run independently.
	InboundEnabled  bool   `json:"inboundEnabled"`
	IMAPHost        string `json:"imapHost"`        // typically "imap.<provider>"
	IMAPPort        int    `json:"imapPort"`        // 993 (TLS, default) or 143 (plain)
	IMAPUseTLS      bool   `json:"imapUseTLS"`      // true for IMAPS; default true
	IMAPMailbox     string `json:"imapMailbox"`     // default "INBOX"
	IMAPPollSeconds int    `json:"imapPollSeconds"` // default 30; clamped to ≥5
}

// SignalChannelConfig configures outbound delivery via signal-cli-rest-api.
// The operator runs that sidecar separately; we just POST to its /v2/send.
type SignalChannelConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseUrl"` // e.g. "http://127.0.0.1:8080"
	Number  string `json:"number"`  // bot's own number, "+15551234567"
}

// MatrixChannelConfig configures outbound to a Matrix homeserver via the
// Client-Server API. Target room ids (not aliases) are passed per-message.
type MatrixChannelConfig struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"baseUrl"`     // e.g. "https://matrix.example.com"
	AccessToken string `json:"accessToken"` // bot's Matrix access token
}

// MattermostChannelConfig configures outbound to a Mattermost v4 API.
type MattermostChannelConfig struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"baseUrl"`     // "https://mattermost.example.com"
	AccessToken string `json:"accessToken"` // personal access token / bot token
}

type ChannelsConfig struct {
	Webhook    WebhookChannelConfig    `json:"webhook"`
	Telegram   TelegramChannelConfig   `json:"telegram"`
	Slack      SlackChannelConfig      `json:"slack"`
	Discord    DiscordChannelConfig    `json:"discord"`
	Teams      TeamsChannelConfig      `json:"teams"`
	WhatsApp   WhatsAppChannelConfig   `json:"whatsapp"`
	Line       LineChannelConfig       `json:"line"`
	Nostr      NostrChannelConfig      `json:"nostr"`
	Email      EmailChannelConfig      `json:"email"`
	Signal     SignalChannelConfig     `json:"signal"`
	Matrix     MatrixChannelConfig     `json:"matrix"`
	Mattermost MattermostChannelConfig `json:"mattermost"`
}

// MCPServerConfig describes an MCP server endpoint.
type MCPServerConfig struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	APIKey  string `json:"apiKey"`
	Enabled bool   `json:"enabled"`
}

// SkillConfig describes a named capability / skill the agent can use.
type SkillConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Endpoint    string `json:"endpoint"` // HTTP endpoint for skill invocation
	Enabled     bool   `json:"enabled"`
}

// MemoryConfig controls agent memory (context window management).
type MemoryConfig struct {
	MaxMessages        int  `json:"maxMessages"`  // 0 = unlimited
	CompactAfter       int  `json:"compactAfter"` // compact when > N messages
	SummarizeOnCompact bool `json:"summarizeOnCompact"`
}

// NodeConfig describes a remote gateway node (multi-node setup).
type NodeConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	APIKey  string `json:"apiKey"`
	Enabled bool   `json:"enabled"`
}

// Config is the full openclaw-go configuration schema.
type Config struct {
	Gateway   GatewayConfig     `json:"gateway"`
	Agent     AgentConfig       `json:"agent"`
	Providers ProvidersConfig   `json:"providers"`
	Channels  ChannelsConfig    `json:"channels"`
	Memory    MemoryConfig      `json:"memory"`
	MCP       []MCPServerConfig `json:"mcp"`
	Skills    []SkillConfig     `json:"skills"`
	Nodes     []NodeConfig      `json:"nodes"`
}

func Default() Config {
	return Config{
		Gateway: GatewayConfig{
			Host: "127.0.0.1",
			Port: 18789,
			AllowedOrigins: []string{
				"http://127.0.0.1",
				"http://localhost",
			},
		},
		Agent: AgentConfig{
			Model:    "echo",
			Provider: "echo",
		},
		Providers: ProvidersConfig{
			OpenAI: OpenAIConfig{
				Model: "gpt-4o-mini",
			},
			Anthropic: AnthropicConfig{
				Model: "claude-3-5-haiku-20241022",
			},
		},
		Channels: ChannelsConfig{
			Webhook: WebhookChannelConfig{
				Enabled: false,
			},
			Telegram: TelegramChannelConfig{
				Enabled:     false,
				InboundMode: "polling",
				WebhookPath: "/webhooks/telegram",
			},
			Slack: SlackChannelConfig{
				Enabled:     false,
				InboundMode: "webhook",
				WebhookPath: "/webhooks/slack",
			},
			Discord: DiscordChannelConfig{
				Enabled:     false,
				InboundMode: "webhook",
				WebhookPath: "/webhooks/discord",
			},
			Teams: TeamsChannelConfig{
				Enabled:     false,
				InboundMode: "webhook",
				WebhookPath: "/webhooks/teams",
			},
			WhatsApp: WhatsAppChannelConfig{
				Enabled:     false,
				InboundMode: "webhook",
				WebhookPath: "/webhooks/whatsapp",
			},
			Line: LineChannelConfig{
				Enabled:     false,
				WebhookPath: "/webhooks/line",
			},
			Nostr: NostrChannelConfig{
				Enabled: false,
			},
			Email: EmailChannelConfig{
				Enabled:         false,
				Port:            587,
				IMAPPort:        993,
				IMAPUseTLS:      true,
				IMAPMailbox:     "INBOX",
				IMAPPollSeconds: 30,
			},
			Signal: SignalChannelConfig{
				Enabled: false,
			},
			Matrix: MatrixChannelConfig{
				Enabled: false,
			},
			Mattermost: MattermostChannelConfig{
				Enabled: false,
			},
		},
	}
}

func DefaultPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_PATH")); p != "" {
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".openclaw-go", "openclaw.json"), nil
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return cfg, err
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return finalizeLoadedConfig(Default()), nil
		}
		return cfg, err
	}
	if len(raw) == 0 {
		return finalizeLoadedConfig(Default()), nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return finalizeLoadedConfig(cfg), nil
}

// finalizeLoadedConfig applies defaults and environment overrides after loading JSON (or when no file exists).
func finalizeLoadedConfig(cfg Config) Config {
	if cfg.Gateway.Host == "" {
		cfg.Gateway.Host = "127.0.0.1"
	}
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = 18789
	}
	if h := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_HOST")); h != "" {
		cfg.Gateway.Host = h
	}
	if cfg.Gateway.AuthToken == "" {
		cfg.Gateway.AuthToken = os.Getenv("OPENCLAW_GATEWAY_AUTH_TOKEN")
	}
	if cfg.Gateway.PluginsDir == "" {
		cfg.Gateway.PluginsDir = os.Getenv("OPENCLAW_PLUGINS_DIR")
	}
	if cfg.Gateway.Password == "" {
		cfg.Gateway.Password = os.Getenv("OPENCLAW_GATEWAY_PASSWORD")
	}
	if len(cfg.Gateway.AllowedOrigins) == 0 {
		origins := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_ALLOWED_ORIGINS"))
		if origins != "" {
			items := strings.Split(origins, ",")
			cfg.Gateway.AllowedOrigins = make([]string, 0, len(items))
			for _, item := range items {
				trimmed := strings.TrimSpace(item)
				if trimmed != "" {
					cfg.Gateway.AllowedOrigins = append(cfg.Gateway.AllowedOrigins, trimmed)
				}
			}
		}
	}
	if len(cfg.Gateway.AllowedOrigins) == 0 {
		cfg.Gateway.AllowedOrigins = []string{
			"http://127.0.0.1",
			"http://localhost",
		}
	}
	if cfg.Agent.Model == "" {
		cfg.Agent.Model = "echo"
	}
	if cfg.Agent.Provider == "" {
		cfg.Agent.Provider = "echo"
	}
	if cfg.Providers.OpenAI.Model == "" {
		cfg.Providers.OpenAI.Model = "gpt-4o-mini"
	}
	if cfg.Providers.OpenAI.APIKey == "" {
		cfg.Providers.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if cfg.Providers.OpenAI.BaseURL == "" {
		cfg.Providers.OpenAI.BaseURL = os.Getenv("OPENAI_BASE_URL")
	}
	if cfg.Providers.Anthropic.APIKey == "" {
		cfg.Providers.Anthropic.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if cfg.Providers.Anthropic.BaseURL == "" {
		cfg.Providers.Anthropic.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if cfg.Providers.Anthropic.Model == "" {
		cfg.Providers.Anthropic.Model = "claude-3-5-haiku-20241022"
	}
	if cfg.Channels.Telegram.BotToken == "" {
		cfg.Channels.Telegram.BotToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if cfg.Channels.Telegram.ChatID == "" {
		cfg.Channels.Telegram.ChatID = os.Getenv("TELEGRAM_CHAT_ID")
	}
	if cfg.Channels.Telegram.InboundMode == "" {
		cfg.Channels.Telegram.InboundMode = "polling"
	}
	if cfg.Channels.Telegram.WebhookPath == "" {
		cfg.Channels.Telegram.WebhookPath = "/webhooks/telegram"
	}
	if cfg.Channels.Telegram.WebhookSecret == "" {
		cfg.Channels.Telegram.WebhookSecret = os.Getenv("TELEGRAM_WEBHOOK_SECRET")
	}
	if cfg.Channels.Slack.BotToken == "" {
		cfg.Channels.Slack.BotToken = os.Getenv("SLACK_BOT_TOKEN")
	}
	if cfg.Channels.Slack.ChannelID == "" {
		cfg.Channels.Slack.ChannelID = os.Getenv("SLACK_CHANNEL_ID")
	}
	if cfg.Channels.Slack.InboundMode == "" {
		cfg.Channels.Slack.InboundMode = "webhook"
	}
	if cfg.Channels.Slack.WebhookPath == "" {
		cfg.Channels.Slack.WebhookPath = "/webhooks/slack"
	}
	if cfg.Channels.Slack.SigningSecret == "" {
		cfg.Channels.Slack.SigningSecret = os.Getenv("SLACK_SIGNING_SECRET")
	}
	if cfg.Channels.Discord.BotToken == "" {
		cfg.Channels.Discord.BotToken = os.Getenv("DISCORD_BOT_TOKEN")
	}
	if cfg.Channels.Discord.ChannelID == "" {
		cfg.Channels.Discord.ChannelID = os.Getenv("DISCORD_CHANNEL_ID")
	}
	if cfg.Channels.Discord.InboundMode == "" {
		cfg.Channels.Discord.InboundMode = "webhook"
	}
	if cfg.Channels.Discord.WebhookPath == "" {
		cfg.Channels.Discord.WebhookPath = "/webhooks/discord"
	}
	if cfg.Channels.Discord.WebhookToken == "" {
		cfg.Channels.Discord.WebhookToken = os.Getenv("DISCORD_WEBHOOK_TOKEN")
	}
	if cfg.Channels.Teams.OutboundURL == "" {
		cfg.Channels.Teams.OutboundURL = os.Getenv("TEAMS_OUTBOUND_WEBHOOK_URL")
	}
	if cfg.Channels.Teams.InboundMode == "" {
		cfg.Channels.Teams.InboundMode = "webhook"
	}
	if cfg.Channels.Teams.WebhookPath == "" {
		cfg.Channels.Teams.WebhookPath = "/webhooks/teams"
	}
	if cfg.Channels.Teams.WebhookSecret == "" {
		cfg.Channels.Teams.WebhookSecret = os.Getenv("TEAMS_WEBHOOK_SECRET")
	}
	if cfg.Channels.WhatsApp.AccessToken == "" {
		cfg.Channels.WhatsApp.AccessToken = os.Getenv("WHATSAPP_ACCESS_TOKEN")
	}
	if cfg.Channels.WhatsApp.PhoneNumberID == "" {
		cfg.Channels.WhatsApp.PhoneNumberID = os.Getenv("WHATSAPP_PHONE_NUMBER_ID")
	}
	if cfg.Channels.WhatsApp.ToNumber == "" {
		cfg.Channels.WhatsApp.ToNumber = os.Getenv("WHATSAPP_TO_NUMBER")
	}
	if cfg.Channels.WhatsApp.InboundMode == "" {
		cfg.Channels.WhatsApp.InboundMode = "webhook"
	}
	if cfg.Channels.WhatsApp.WebhookPath == "" {
		cfg.Channels.WhatsApp.WebhookPath = "/webhooks/whatsapp"
	}
	if cfg.Channels.WhatsApp.VerifyToken == "" {
		cfg.Channels.WhatsApp.VerifyToken = os.Getenv("WHATSAPP_VERIFY_TOKEN")
	}
	if cfg.Channels.WhatsApp.AppSecret == "" {
		cfg.Channels.WhatsApp.AppSecret = os.Getenv("WHATSAPP_APP_SECRET")
	}
	if cfg.Channels.Line.ChannelToken == "" {
		cfg.Channels.Line.ChannelToken = os.Getenv("LINE_CHANNEL_TOKEN")
	}
	if cfg.Channels.Line.ChannelSecret == "" {
		cfg.Channels.Line.ChannelSecret = os.Getenv("LINE_CHANNEL_SECRET")
	}
	if cfg.Channels.Line.WebhookPath == "" {
		cfg.Channels.Line.WebhookPath = "/webhooks/line"
	}
	if cfg.Channels.Nostr.RelayURL == "" {
		cfg.Channels.Nostr.RelayURL = os.Getenv("NOSTR_RELAY_URL")
	}
	if cfg.Channels.Nostr.Pubkey == "" {
		cfg.Channels.Nostr.Pubkey = os.Getenv("NOSTR_PUBKEY")
	}
	return cfg
}

func Save(path string, cfg Config) error {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(path, raw, 0o600)
}
