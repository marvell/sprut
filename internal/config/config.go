// Package config turns a Config file into the Upstream definitions it declares.
package config

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Upstream is one resolved Upstream definition from the Config.
type Upstream struct {
	Name    string
	Command string
	Args    []string
}

type file struct {
	MCPServers map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcpServers"`
}

// Parse parses Config bytes into Upstreams, ordered by name.
func Parse(data []byte) ([]Upstream, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	upstreams := make([]Upstream, 0, len(f.MCPServers))
	for name, s := range f.MCPServers {
		upstreams = append(upstreams, Upstream{Name: name, Command: s.Command, Args: s.Args})
	}
	sort.Slice(upstreams, func(i, j int) bool { return upstreams[i].Name < upstreams[j].Name })
	return upstreams, nil
}
