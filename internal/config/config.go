// Package config turns a Config file into the Upstream definitions it declares.
package config

import (
	"cmp"
	"encoding/json"
	"slices"
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
		return nil, err
	}
	upstreams := make([]Upstream, 0, len(f.MCPServers))
	for name, s := range f.MCPServers {
		upstreams = append(upstreams, Upstream{Name: name, Command: s.Command, Args: s.Args})
	}
	slices.SortFunc(upstreams, func(a, b Upstream) int { return cmp.Compare(a.Name, b.Name) })
	return upstreams, nil
}
