package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-kratos/blades"
)

const agentsFileName = "AGENTS.md"

func workspaceAgentsPromptMiddleware(workspace string) blades.Middleware {
	return func(next blades.Handler) blades.Handler {
		return blades.HandleFunc(func(ctx context.Context, invocation *blades.Invocation) blades.Generator[*blades.Message, error] {
			if prompt := readWorkspaceAgentsPrompt(workspace); prompt != "" {
				if invocation.Instruction == nil {
					invocation.Instruction = blades.SystemMessage(prompt)
				} else {
					invocation.Instruction = blades.MergeParts(invocation.Instruction, blades.SystemMessage(prompt))
				}
			}
			return next.Handle(ctx, invocation)
		})
	}
}

func readWorkspaceAgentsPrompt(workspace string) string {
	promptFile := filepath.Join(workspace, agentsFileName)
	content, err := os.ReadFile(promptFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}
