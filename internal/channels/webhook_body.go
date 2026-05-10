package channels

import (
	"io"
	"net/http"
	"strings"
)

// MaxWebhookBodyBytes is the maximum POST body size accepted by channel webhooks
// (aligned with the gateway JSON body limit).
const MaxWebhookBodyBytes int64 = 4 << 20

// readWebhookBody reads r.Body capped at MaxWebhookBodyBytes. On overflow,
// http.MaxBytesReader writes the HTTP response (typically 413); callers should
// return without writing headers again when errBodyTooLarge(err) is true.
func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxWebhookBodyBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		if errBodyTooLarge(err) {
			return nil, err
		}
		return nil, err
	}
	return b, nil
}

func errBodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "body too large")
}
