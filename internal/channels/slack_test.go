package channels

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSlackChallengeJSONInjection verifies that a challenge containing JSON
// special characters does not produce malformed JSON in the response.
func TestSlackChallengeJSONInjection(t *testing.T) {
	malicious := `evil"injection`

	// Build the test payload using json.Marshal so the challenge is properly
	// escaped in the input JSON — we're testing the OUTPUT escaping.
	inputPayload, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"challenge": malicious,
	})
	raw := inputPayload

	replyBody, msgs, err := decodeSlackEvents(raw)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatal("url_verification should produce no inbound messages")
	}

	// The reply body must be valid JSON.
	var parsed map[string]string
	if err := json.Unmarshal([]byte(replyBody), &parsed); err != nil {
		t.Fatalf("reply body is not valid JSON %q: %v", replyBody, err)
	}
	// The challenge value must be preserved exactly.
	if parsed["challenge"] != malicious {
		t.Fatalf("challenge not preserved: got %q, want %q", parsed["challenge"], malicious)
	}
}

// TestSlackChallengeNormalValue verifies that a plain challenge round-trips correctly.
func TestSlackChallengeNormalValue(t *testing.T) {
	challenge := "abc123XYZ"
	raw := []byte(`{"type":"url_verification","challenge":"` + challenge + `"}`)

	replyBody, _, err := decodeSlackEvents(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyBody, challenge) {
		t.Fatalf("challenge %q not found in reply %q", challenge, replyBody)
	}
}
