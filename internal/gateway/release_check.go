package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultGitHubUpdateOwner = "niyogen"
const defaultGitHubUpdateRepo = "openclaw-go"

// githubReleaseAPI is the GitHub REST response shape we read (subset).
type githubReleaseAPI struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// fetchGitHubLatestRelease performs a GET to apiURL (typically .../releases/latest)
// and parses tag_name / html_url. owner/repo are used only to synthesize releasesPage
// when html_url is empty.
func fetchGitHubLatestRelease(ctx context.Context, apiURL, owner, repo string) (tag, pageURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "openclaw-go/update-check")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("github api: %s", strings.TrimSpace(string(body)))
	}
	var decoded githubReleaseAPI
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", err
	}
	tag = strings.TrimSpace(decoded.TagName)
	page := strings.TrimSpace(decoded.HTMLURL)
	if page == "" && owner != "" && repo != "" {
		page = fmt.Sprintf("https://github.com/%s/%s/releases/latest", owner, repo)
	}
	return tag, page, nil
}

// FetchUpstreamRelease asks api.github.com for the latest published release tag.
func FetchUpstreamRelease(ctx context.Context, owner, repo string) (tag, pageURL string, err error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("owner and repo are required")
	}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	return fetchGitHubLatestRelease(ctx, apiURL, owner, repo)
}

// UpdateAvailable reports whether latestTag is semantically newer than current.
func UpdateAvailable(current, latestTag string) bool {
	latestTag = strings.TrimSpace(latestTag)
	current = strings.TrimSpace(current)
	if latestTag == "" {
		return false
	}
	if current == "" || strings.EqualFold(current, "dev") {
		return false
	}
	return semverCmp(stripV(latestTag), stripV(current)) > 0
}

func stripV(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}

// semverCmp compares dot-separated numeric semver pieces (major.minor.patch…).
// Returns -1 if a<b, 0 if equal, 1 if a>b. Non-numeric tail segments are ignored.
func semverCmp(a, b string) int {
	ap := parseSemverInts(a)
	bp := parseSemverInts(b)
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(ap) {
			ai = ap[i]
		}
		if i < len(bp) {
			bi = bp[i]
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

func parseSemverInts(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Stop at first non-numeric segment (e.g. pre-release tag).
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// FetchDefaultRepoLatestRelease resolves the newest version for the bundled repo.
// It prefers a formal GitHub “latest release”; if that is missing (404) or returns
// no tag, it falls back to the first entry from the tags API (lightweight tags).
func FetchDefaultRepoLatestRelease(ctx context.Context) (tag, pageURL string, err error) {
	owner, repo := defaultGitHubUpdateOwner, defaultGitHubUpdateRepo
	tag, pageURL, err = FetchUpstreamRelease(ctx, owner, repo)
	if err == nil && strings.TrimSpace(tag) != "" {
		return tag, pageURL, nil
	}
	tag2, page2, err2 := fetchGitHubFirstTag(ctx, owner, repo)
	if err2 == nil && strings.TrimSpace(tag2) != "" {
		if strings.TrimSpace(page2) == "" {
			page2 = fmt.Sprintf("https://github.com/%s/%s/tags", owner, repo)
		}
		return tag2, page2, nil
	}
	if err != nil && err2 != nil {
		return "", "", fmt.Errorf("latest release: %v; tags: %w", err, err2)
	}
	if err != nil {
		return "", "", err
	}
	return "", "", err2
}

type githubTagEntry struct {
	Name string `json:"name"`
}

func fetchGitHubFirstTag(ctx context.Context, owner, repo string) (tag, pageURL string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/tags?per_page=1", owner, repo)
	return fetchGitHubTagList(ctx, apiURL, owner, repo)
}

func fetchGitHubTagList(ctx context.Context, apiURL, owner, repo string) (tag, pageURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "openclaw-go/update-check")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("github api: %s", strings.TrimSpace(string(body)))
	}
	var tags []githubTagEntry
	if err := json.Unmarshal(body, &tags); err != nil {
		return "", "", err
	}
	if len(tags) == 0 {
		return "", "", fmt.Errorf("no tags returned")
	}
	tag = strings.TrimSpace(tags[0].Name)
	pageURL = fmt.Sprintf("https://github.com/%s/%s/tags", owner, repo)
	return tag, pageURL, nil
}

// LatestReleaseCheckFn is invoked by update.status / update.run. Tests may replace it
// to avoid network I/O while still exercising RPC handlers.
var LatestReleaseCheckFn = FetchDefaultRepoLatestRelease
