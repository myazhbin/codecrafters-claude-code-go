package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBashToolReturnsCommandOutput(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"stdout", `echo hello`, "hello\n"},
		{"stderr", `echo oops >&2`, "oops\n"},
		{"stdout and stderr in order", `echo first; echo second >&2; echo third`, "first\nsecond\nthird\n"},
		{"shell features", `printf '%s\n' a b c | wc -l | tr -d ' '`, "3\n"},
		{"no output", `true`, "(no output)"},
		{"stdin is empty", `cat`, "(no output)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bashTool.Run(context.Background(), jsonArgs(t, map[string]any{"command": tt.command}))

			if err != nil {
				t.Fatalf("Bash returned an error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Bash = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBashToolRunsInWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "current readme\n")
	writeFile(t, filepath.Join(dir, "README_old.md"), "old readme\n")
	t.Chdir(dir)

	got, err := bashTool.Run(context.Background(), `{"command": "rm README_old.md && ls"}`)

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if got != "README.md\n" {
		t.Errorf("Bash = %q, want the listing of the working directory %q", got, "README.md\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "README_old.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("README_old.md still exists (stat error: %v)", err)
	}
	if got := readFile(t, filepath.Join(dir, "README.md")); got != "current readme\n" {
		t.Errorf("README.md contains %q, want it untouched", got)
	}
}

func TestBashToolReportsFailedCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"exit code only", `exit 3`, "Exit code: 3"},
		{"output then exit code", `echo partial; echo broken >&2; exit 2`, "partial\nbroken\nExit code: 2"},
		{"output without trailing newline", `printf 'no newline'; exit 1`, "no newline\nExit code: 1"},
		{"killed by a signal", `kill -TERM $$`, "Terminated: signal: terminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bashTool.Run(context.Background(), jsonArgs(t, map[string]any{"command": tt.command}))

			if err != nil {
				t.Fatalf("Bash returned an error, want the failure reported as its result: %v", err)
			}
			if got != tt.want {
				t.Errorf("Bash = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBashToolReportsUnknownCommand(t *testing.T) {
	got, err := bashTool.Run(context.Background(), `{"command": "definitely-not-a-real-command-xyz"}`)

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if !strings.Contains(got, "not found") || !strings.HasSuffix(got, "Exit code: 127") {
		t.Errorf("Bash = %q, want the shell's \"not found\" message and exit code 127", got)
	}
}

func TestBashToolTimesOutAndKillsTheWholeCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The shell starts a child and waits for it: both must die on timeout.
	command := `echo started; sleep 30 & echo $! > ` + shellQuote(pidFile) + `; wait`
	tool := newBashTool(300 * time.Millisecond)

	start := time.Now()
	got, err := tool.Run(context.Background(), jsonArgs(t, map[string]any{"command": command}))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if want := "started\nCommand timed out after 300ms"; got != want {
		t.Errorf("Bash = %q, want %q", got, want)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Bash took %s to give up on a command with a 300ms timeout", elapsed)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err != nil {
		t.Fatalf("cannot read the child's pid: %v", err)
	}
	if !processExitsWithin(pid, 5*time.Second) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child process %d survived the timeout", pid)
	}
}

func TestBashToolDoesNotWaitForBackgroundProcesses(t *testing.T) {
	// The background process inherits the output pipe and keeps it open.
	start := time.Now()
	got, err := bashTool.Run(context.Background(), `{"command": "sleep 30 & echo $!"}`)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(got))
	if convErr != nil {
		t.Fatalf("Bash = %q, want the pid of the background process", got)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if elapsed > 10*time.Second {
		t.Errorf("Bash took %s: it waited for the background process", elapsed)
	}
}

func TestBashToolStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	_, err := bashTool.Run(ctx, `{"command": "sleep 30"}`)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Bash took %s to stop after the context was cancelled", elapsed)
	}
}

func TestBashToolTruncatesHugeOutput(t *testing.T) {
	// 200,000 bytes of output: a line of 'a's, a middle of 'x's, a line of 'z's.
	command := `printf 'aaaaaaaaa\n'; head -c 199980 /dev/zero | tr '\0' 'x'; printf '\nzzzzzzzzz'`

	got, err := bashTool.Run(context.Background(), jsonArgs(t, map[string]any{"command": command}))

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if len(got) > bashMaxOutput+100 {
		t.Errorf("Bash returned %d bytes, want about %d at most", len(got), bashMaxOutput)
	}
	if !strings.HasPrefix(got, "aaaaaaaaa\n") || !strings.HasSuffix(got, "\nzzzzzzzzz") {
		t.Errorf("Bash = %q...%q, want the beginning and the end of the output kept", got[:20], got[len(got)-20:])
	}
	if want := "[170000 bytes truncated]"; !strings.Contains(got, want) {
		t.Errorf("Bash result does not say how much was cut, want it to contain %q", want)
	}
}

func TestBashToolRejectsBadCalls(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		wantErr   string
	}{
		{"malformed JSON", `{"command": `, "invalid arguments"},
		{"empty arguments", ``, "invalid arguments"},
		{"wrong argument type", `{"command": ["ls"]}`, "invalid arguments"},
		{"missing command", `{}`, "command is required"},
		{"blank command", `{"command": "  \n"}`, "command is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bashTool.Run(context.Background(), tt.arguments)

			if err == nil {
				t.Fatalf("Bash = %q, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestTruncateMiddle(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{"shorter than the limit", "short", 10, "short"},
		{"exactly the limit", "0123456789", 10, "0123456789"},
		{"longer than the limit", "0123456789ABCDEF", 10, "01234\n\n... [6 bytes truncated] ...\n\nBCDEF"},
		// Each é is two bytes: both cuts land in the middle of one.
		{"cuts through multi-byte characters", "ééééé", 6, "é\n\n... [4 bytes truncated] ...\n\né"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateMiddle(tt.text, tt.limit)

			if got != tt.want {
				t.Errorf("truncateMiddle(%q, %d) = %q, want %q", tt.text, tt.limit, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateMiddle(%q, %d) = %q is not valid UTF-8", tt.text, tt.limit, got)
			}
		})
	}
}

// shellQuote quotes a string for use as a single word in a shell command.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// processExitsWithin reports whether the process is gone before the deadline.
func processExitsWithin(pid int, deadline time.Duration) bool {
	for end := time.Now().Add(deadline); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}
