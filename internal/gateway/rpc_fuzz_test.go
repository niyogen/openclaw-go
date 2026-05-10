package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzRPCRequestDecode(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"health","params":{}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxBodyBytes {
			data = data[:maxBodyBytes]
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			return
		}
		_ = req.Method
		_ = req.Params
	})
}
