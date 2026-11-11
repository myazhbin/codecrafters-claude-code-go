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
	// system is the system prompt that opens the conversation, if any.
	system string
	// feedback, if set, is asked before each request to the model for what
	// has turned up since the previous one, such as the issues found in the
	// code that the model edited. The model gets it with its latest tool
	// result.
	feedback func() string
	// log receives progress information for humans. It is never stdout, which
	// is reserved for the final answer.
	log io.Writer
}

// Run executes the agent loop for the user's prompts, each of which opens the
// conversation as a message of its own, and returns the model's final answer.
func (a *Agent) Run(ctx context.Context, prompts []string) (string, error) {
	tools := a.toolbox.Specs()
	var messages []openai.ChatCompletionMessageParamUnion
	if a.system != "" {
		messages = append(messages, openai.SystemMessage(a.system))
	}
	for _, prompt := range prompts {
		messages = append(messages, openai.UserMessage(prompt))
	}

	for {
		a.deliverFeedback(messages)
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

// deliverFeedback adds the feedback that has turned up since the last request
// to the latest tool result of the conversation. Feedback is only asked for
// once there is a tool result to carry it.
func (a *Agent) deliverFeedback(messages []openai.ChatCompletionMessageParamUnion) {
	if a.feedback == nil {
		return
	}
	result := messages[len(messages)-1].OfTool
	if result == nil {
		return
	}
	feedback := a.feedback()
	if feedback == "" {
		return
	}
	if content := result.Content.OfString.Value; content != "" {
		feedback = content + "\n\n" + feedback
	}
	result.Content.OfString = openai.String(feedback)
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
