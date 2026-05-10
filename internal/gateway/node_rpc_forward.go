package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openclaw-go/internal/topology"
)

const (
	maxNodeRPCAttempts = 4 // initial try + 3 retries (matches channel router spirit)
	nodePerAttemptTO   = 12 * time.Second
	nodeHTTPClientTO   = 15 * time.Second
)

// forwardNodeRPC posts JSON-RPC 2.0 to the remote gateway /rpc with retries on
// transport failures and selected HTTP status codes (5xx, 429, 408).
func forwardNodeRPC(ctx context.Context, node topology.Node, method string, params json.RawMessage) (any, *rpcError) {
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, &rpcError{Code: -32602, Message: "method is required"}
	}
	base := strings.TrimSpace(node.URL)
	if base == "" {
		return nil, &rpcError{Code: -32602, Message: "node url is empty"}
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &rpcError{Code: -32602, Message: "node url must be http or https with a host"}
	}
	rpcURL := strings.TrimRight(base, "/") + "/rpc"

	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage(`{}`)
	}
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: err.Error()}
	}

	var last *rpcError
	for attempt := 0; attempt < maxNodeRPCAttempts; attempt++ {
		if attempt > 0 {
			shift := uint(attempt - 1)
			if shift > 5 {
				shift = 5
			}
			backoff := time.Duration(200<<shift) * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("node invoke cancelled after %d attempts: %v", attempt, ctx.Err())}
			case <-time.After(backoff):
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, nodePerAttemptTO)
		res, rpcErr, retry := forwardNodeRPCOnce(attemptCtx, rpcURL, raw, node)
		cancel()

		if rpcErr == nil {
			return res, nil
		}
		last = rpcErr
		if !retry {
			return nil, last
		}
	}
	if last == nil {
		return nil, &rpcError{Code: -32000, Message: "node invoke failed"}
	}
	return nil, &rpcError{
		Code:    last.Code,
		Message: fmt.Sprintf("%s (peer unavailable after %d attempts)", last.Message, maxNodeRPCAttempts),
	}
}

func forwardNodeRPCOnce(
	ctx context.Context,
	rpcURL string,
	raw []byte,
	node topology.Node,
) (any, *rpcError, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(raw))
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}, true
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(node.APIKey); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: nodeHTTPClientTO}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "node transport: " + err.Error()}, true
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "node read body: " + err.Error()}, true
	}

	if shouldRetryNodeHTTPStatus(resp.StatusCode) {
		return nil, &rpcError{
			Code:    -32000,
			Message: fmt.Sprintf("node HTTP %d: %s", resp.StatusCode, truncateForErr(respBody, 512)),
		}, true
	}

	if resp.StatusCode >= 400 {
		return nil, &rpcError{
			Code:    -32000,
			Message: fmt.Sprintf("node HTTP %d: %s", resp.StatusCode, truncateForErr(respBody, 512)),
		}, false
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, &rpcError{Code: -32000, Message: "invalid json from node: " + err.Error()}, false
	}
	if envelope.Error != nil {
		return nil, envelope.Error, false
	}
	if len(envelope.Result) == 0 {
		return map[string]any{}, nil, false
	}
	var parsed any
	if err := json.Unmarshal(envelope.Result, &parsed); err != nil {
		return json.RawMessage(envelope.Result), nil, false
	}
	return parsed, nil, false
}

func shouldRetryNodeHTTPStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return code >= 500 && code <= 599
	}
}

func truncateForErr(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
