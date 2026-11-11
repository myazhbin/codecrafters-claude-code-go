package main

import (
	"context"
	"fmt"
	"io"

	"github.com/openai/openai-go/v3"
)

// connectLSP connects to the language server that the environment configures
// and returns its client, or nil if there is none to use: the agent then makes
// do with its other tools.
func connectLSP(log io.Writer) *LSPClient {
	client, err := LSPClientFromEnvironment()
	if err != nil {
		fmt.Fprintf(log, "warning: LSP is unavailable: %v\n", err)
		return nil
	}
	if client != nil {
		fmt.Fprintln(log, "LSP client connected")
	}
	return client
}

// newLSPTool returns the tool that lets the model ask the language server
// about the code of the project.
func newLSPTool() Tool {
	return Tool{
		Name:        "LSP",
		Description: "Returns the file, line, and character where a symbol is defined.",
		Parameters: openai.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "string",
					"enum": []string{"goToDefinition"},
				},
				"filePath": map[string]any{
					"type":        "string",
					"description": "Absolute path, or a path relative to the agent's current working directory.",
				},
				"line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line number of the symbol.",
				},
				"character": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based character offset in UTF-16 code units (same as character count for ASCII).",
				},
			},
			"required": []string{"operation", "filePath", "line", "character"},
		},
		Run: func(context.Context, string) (string, error) {
			return "LSP is not implemented yet", nil
		},
	}
}
