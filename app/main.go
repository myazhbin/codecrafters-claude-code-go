package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const (
	// defaultBaseURL is used when OPENROUTER_BASE_URL is not set.
	defaultBaseURL = "https://openrouter.ai/api/v1"
	// model is the LLM that every request is sent to.
	model = "anthropic/claude-haiku-4.5"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is the program's testable entry point. It sends the prompt given with -p
// to the LLM and writes the outcome to stdout; anything else (usage, errors)
// goes to stderr. It returns the process exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("claude-code", flag.ContinueOnError)
	flags.SetOutput(stderr)
	prompt := flags.String("p", "", "Prompt to send to LLM")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *prompt == "" {
		fmt.Fprintln(stderr, "error: prompt must not be empty (pass it with -p)")
		return 1
	}

	apiKey := getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(stderr, "error: env variable OPENROUTER_API_KEY not found")
		return 1
	}
	baseURL := getenv("OPENROUTER_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	client := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))
	output, err := complete(ctx, client, defaultToolbox(), *prompt)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprint(stdout, output)
	return 0
}

// complete sends the prompt to the LLM, advertising the tools in the toolbox.
// If the model answers with a tool call, the tool is executed and its result
// is returned; otherwise the text of the model's reply is returned.
func complete(ctx context.Context, client openai.Client, toolbox Toolbox, prompt string) (string, error) {
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
		Tools:    toolbox.Specs(),
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("no choices in response")
	}

	message := resp.Choices[0].Message
	if len(message.ToolCalls) == 0 {
		return message.Content, nil
	}
	// Only the first tool call is executed for now, and its result is the
	// program's output.
	call := message.ToolCalls[0].Function
	return toolbox.Call(call.Name, call.Arguments)
}
