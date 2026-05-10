//go:build integration

package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Hits api.github.com — run with: go test -tags=integration ./internal/gateway/...
func TestIntegrationFetchDefaultRepoLatestReleaseLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	tag, page, err := FetchDefaultRepoLatestRelease(ctx)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "404") || strings.Contains(msg, "no tags") {
			t.Skipf("default repo has no GitHub release or tags yet: %v", err)
		}
		t.Fatalf("live github: %v", err)
	}
	if strings.TrimSpace(tag) == "" {
		t.Fatal("empty tag_name from GitHub")
	}
	if strings.TrimSpace(page) == "" {
		t.Fatal("empty releases page URL")
	}
}
