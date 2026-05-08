package gateway

// Version is injected at build time via:
//
//	go build -ldflags "-X openclaw-go/internal/gateway.Version=<ver>" ./cmd/openclaw
//
// Falls back to "dev" when built without the flag.
var Version = "dev"
