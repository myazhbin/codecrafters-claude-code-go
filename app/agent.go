package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/openai/openai-go/v3"
)

// Agent drives a conversation with the model: it sends the conversation so
// far, executes the tools the model asks for, and feeds their results back
// until the model answers without requesting any tool.
type Agent struct {
	client  openai.Client
	toolbox Toolbox
	// log receives progress information for humans. It is never stdout, which
	// is reserved for the final answer.
	log io.Writer
}

// Run executes the agent loop for the prompt and returns the model's final
// answer.
func (a *Agent) Run(ctx context.Context, prompt string) (string, error) {
	tools := a.toolbox.Specs()
	messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)}

	for {
		resp, err := a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    model,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("no choices in response")
		}

		message := resp.Choices[0].Message
		messages = append(messages, message.ToParam())
		if len(message.ToolCalls) == 0 {
			return message.Content, nil
		}

		for _, call := range message.ToolCalls {
			messages = append(messages, openai.ToolMessage(a.execute(ctx, call), call.ID))
		}
	}
}

// execute runs one tool call and returns the content of the tool message that
// reports its outcome to the model. Failures are reported to the model as
// well, so that it can recover from them instead of the whole run aborting.
func (a *Agent) execute(ctx context.Context, call openai.ChatCompletionMessageToolCallUnion) string {
	name, arguments := call.Function.Name, call.Function.Arguments
	fmt.Fprintf(a.log, "[tool] %s %s\n", name, arguments)

	result, err := a.toolbox.Call(ctx, name, arguments)
	if err != nil {
		fmt.Fprintf(a.log, "[tool] %s failed: %v\n", name, err)
		return "Error: " + err.Error()
	}
	return result
}
