package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
	// An interruption cancels the context, which aborts the request in flight
	// and kills the command the agent may be running.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the program's testable entry point. It hands the prompt given with -p
// to the agent and writes the agent's final answer to stdout; anything else
// (usage, progress, errors) goes to stderr. It returns the process exit code.
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

	skills, err := loadSkills(skillsDir)
	if err != nil {
		fmt.Fprintf(stderr, "warning: skills are unavailable: %v\n", err)
	}
	prompts, err := expandSlashCommands(*prompt, skills)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	agent := &Agent{
		client:  openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		toolbox: newToolbox(skills),
		system:  skillsPrompt(skills),
		log:     stderr,
	}
	answer, err := agent.Run(ctx, prompts)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprint(stdout, answer)
	return 0
}
