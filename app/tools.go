package main

import "github.com/openai/openai-go/v3"

// readTool is the specification of the Read tool as advertised to the model.
var readTool = openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
	Name:        "Read",
	Description: openai.String("Read and return the contents of a file"),
	Parameters: openai.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "The path to the file to read",
			},
		},
		"required": []string{"file_path"},
	},
})

// availableTools returns the specification of every tool advertised to the
// model with each request.
func availableTools() []openai.ChatCompletionToolUnionParam {
	return []openai.ChatCompletionToolUnionParam{readTool}
}
