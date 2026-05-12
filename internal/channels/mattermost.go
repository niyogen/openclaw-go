package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MattermostChannel posts messages to a Mattermost server via the v4 REST
// API (`POST /api/v4/posts`). Inbound (outgoing webhooks → us, or the
// WebSocket event stream) is deferred; users can wire an HTTP outgoing
// webhook from Mattermost into the generic webhook channel for now.
type MattermostChannel struct {
	baseURL     string // "https://mattermost.example.com"
	accessToken string // personal access token / bot token
	client      *http.Client
}

func NewMattermostChannel(baseURL, accessToken string) *MattermostChannel {
	return &MattermostChannel{
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		accessToken: strings.TrimSpace(accessToken),
		client:      &http.Client{Timeout: 20 * time.Second},
	}
}

func (m *MattermostChannel) Name() string {
	return "mattermost"
}

// Send posts a single message to the channel identified by message.Target.
// Target is the Mattermost channel ID (alphanumeric, 26 chars). When
// message.ThreadID is non-empty it threads the post as a reply.
func (m *MattermostChannel) Send(ctx context.Context, message OutboundMessage) error {
	if m.baseURL == "" || m.accessToken == "" {
		return nil // disabled
	}
	channelID := strings.TrimSpace(message.Target)
	if channelID == "" {
		return fmt.Errorf("mattermost: target (channel id) is required")
	}
	body := map[string]any{
		"channel_id": channelID,
		"message":    message.Message,
	}
	if root := strings.TrimSpace(message.ThreadID); root != "" {
		body["root_id"] = root
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := m.baseURL + "/api/v4/posts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.accessToken)
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost: POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mattermost: %s returned %d: %s", endpoint, resp.StatusCode, string(respBody))
	}
	return nil
}
