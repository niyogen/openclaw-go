package plugins

import "net/http"

type Plugin interface {
	Name() string
	RegisterRoutes(mux *http.ServeMux)
}

type Registry struct {
	plugins []Plugin
}

func NewRegistry() *Registry {
	return &Registry{
		plugins: []Plugin{},
	}
}

func (r *Registry) Register(plugin Plugin) {
	r.plugins = append(r.plugins, plugin)
}

func (r *Registry) MountRoutes(mux *http.ServeMux) {
	for _, plugin := range r.plugins {
		plugin.RegisterRoutes(mux)
	}
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.plugins))
	for _, plugin := range r.plugins {
		out = append(out, plugin.Name())
	}
	return out
}
