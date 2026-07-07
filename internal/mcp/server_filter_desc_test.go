package mcp

import (
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// TestFilterDescriptions_NoShortDescriptions ensures no tool parameter
// description falls back to the old short form. The contract is: descriptions
// for project/since/before/limit must be detailed enough to guide correct use.
func TestFilterDescriptions_NoShortDescriptions(t *testing.T) {
	srv := &Server{}
	srv.srv = mcpserver.NewMCPServer("test", "0.0.0")
	srv.registerTools()

	tools := srv.srv.ListTools()

	shortProjectDescs := map[string]bool{
		"Project filter": true,
		"Project name":   true,
		"Project":        true,
	}
	shortSinceDesc := "ISO date lower bound"
	shortBeforeDesc := "ISO date upper bound"
	shortLimitDesc := "Max results"

	for _, st := range tools {
		props := st.Tool.InputSchema.Properties
		for propName, propRaw := range props {
			prop, ok := propRaw.(map[string]any)
			if !ok {
				continue
			}
			desc, ok := prop["description"].(string)
			if !ok {
				continue
			}

			switch propName {
			case "project":
				if shortProjectDescs[desc] {
					t.Errorf("tool %q param %q has short description %q — use projectFilterDesc or projectRequiredDesc constant",
						st.Tool.Name, propName, desc)
				}
			case "since":
				if desc == shortSinceDesc {
					t.Errorf("tool %q param %q has short description %q — use sinceDesc constant",
						st.Tool.Name, propName, desc)
				}
			case "before":
				if desc == shortBeforeDesc {
					t.Errorf("tool %q param %q has short description %q — use beforeDesc constant",
						st.Tool.Name, propName, desc)
				}
			case "limit":
				if desc == shortLimitDesc {
					t.Errorf("tool %q param %q has short description %q — use limitDesc constant",
						st.Tool.Name, propName, desc)
				}
			}
		}
	}
}
