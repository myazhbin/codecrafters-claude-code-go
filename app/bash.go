package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
)

const (
	// bashTimeout bounds how long a single command may run.
	bashTimeout = 2 * time.Minute
	// bashMaxOutput bounds how many bytes of a command's output are reported to
	// the model, so that a chatty command cannot flood the conversation.
	bashMaxOutput = 30_000
)

// bashTool executes a shell command in the program's working directory and
// returns everything the command printed.
var bashTool = newBashTool(bashTimeout)

// newBashTool returns a Bash tool whose commands may run for at most timeout.
func newBashTool(timeout time.Duration) Tool {
	return Tool{
		Name:        "Bash",
		Description: "Execute a shell command",
		Parameters: openai.FunctionParameters{
			"type":     "object",
			"required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command to execute",
				},
			},
		},
		Run: func(ctx context.Context, arguments string) (string, error) {
			var args struct {
				Command string `json:"command"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Command) == "" {
				return "", errors.New("command is required")
			}
			return runShell(ctx, args.Command, timeout)
		},
	}
}

// runShell executes a command line with the shell and reports its combined
// stdout and stderr, followed by a note on how it ended unless it simply
// succeeded. A command that fails is not an error of the tool: the model gets
// to see its output and exit code and can react to them.
func runShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, shellPath(), "-c", command)
	cmd.Stdout = &output
	cmd.Stderr = &output
	// The command gets its own process group so that a timeout or an
	// interruption takes down everything it started, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// A background process started by the command keeps the output pipe open
	// after the shell exits: do not wait for it.
	cmd.WaitDelay = time.Second

	err := cmd.Run()
	result := truncateMiddle(output.String(), bashMaxOutput)

	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		if result == "" {
			return "(no output)", nil
		}
		return result, nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return appendLine(result, fmt.Sprintf("Command timed out after %s", timeout)), nil
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.As(err, &exitErr):
		if code := exitErr.ExitCode(); code >= 0 {
			return appendLine(result, fmt.Sprintf("Exit code: %d", code)), nil
		}
		return appendLine(result, "Terminated: "+exitErr.Error()), nil
	default:
		return "", err
	}
}

// shellPath returns the shell that runs the commands: bash where it is
// installed, as the tool's name promises, and the POSIX shell otherwise.
func shellPath() string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "sh"
}

// appendLine adds a line to the end of text, starting it on a line of its own.
func appendLine(text, line string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + line
}

// truncateMiddle shortens text to roughly limit bytes by cutting out its
// middle: the beginning and the end of a command's output are usually the
// parts that matter.
func truncateMiddle(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	head, tail := text[:limit/2], text[len(text)-limit/2:]
	omitted := len(text) - len(head) - len(tail)
	// The cuts may have split multi-byte characters: drop the broken halves.
	return strings.ToValidUTF8(head, "") +
		fmt.Sprintf("\n\n... [%d bytes truncated] ...\n\n", omitted) +
		strings.ToValidUTF8(tail, "")
}
