package server

import (
	"github.com/xiant10/t10-modkit/docsmcp"

	"t10-collab/docs"
)

// Docs serves this repo's docs to coding agents at /docs/mcp, as the workspace's
// contracts/docs describes. Its agent tools are the list /mcp serves.
func Docs(version string) (*docsmcp.Handler, error) {
	return docsmcp.New(docsmcp.Options{
		Component: "t10-collab",
		Summary:   "Work between people and agent teammates: tasks (work items), offers, shared lists with workflows, and typed items a module declares, with agent tools at /mcp.",
		Version:   version,
		Docs:      docs.FS,
		AgentTools: func() []docsmcp.Tool {
			var out []docsmcp.Tool
			for _, t := range New(Deps{}).tools() {
				description := t.description
				if t.agentOnly {
					description += " Only for an agent teammate: a run whose principal is a tenant agent account."
				}
				out = append(out, docsmcp.Tool{Name: t.name, Description: description, Action: t.action, InputSchema: t.schema})
			}
			return out
		},
	})
}
