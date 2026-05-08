package plugins

import (
	"encoding/json"
	"net/http"
)

type MetaPlugin struct {
	registry *Registry
}

func NewMetaPlugin(registry *Registry) *MetaPlugin {
	return &MetaPlugin{registry: registry}
}

func (p *MetaPlugin) Name() string {
	return "meta"
}

func (p *MetaPlugin) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/plugins", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugins": p.registry.Names(),
		})
	})
}
