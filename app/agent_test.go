package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentLoopAnswersUsingToolResults(t *testing.T) {
	const prompt = "Use README.md to determine the chemical expiry period in months. Number only."
	const readme = "# My Project\n\nChemical expiry period: 6 months\n"
	readCall := toolCall{
		id:        "call_abc123",
		name:      "Read",
		arguments: jsonArgs(t, map[string]any{"file_path": writeTempFile(t, "README.md", readme)}),
	}
	llm := newFakeLLM(t,
		assistantToolCalls(readCall),
		assistantText("6"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "6" {
		t.Errorf("stdout = %q, want only the final answer %q", stdout, "6")
	}

	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	assertMessages(t, requests[0], userMessage(prompt))
	assertMessages(t, requests[1],
		userMessage(prompt),
		assistantMessage("", readCall),
		toolMessage("call_abc123", readme),
	)
	for i, request := range requests {
		if tools := advertisedTools(t, request); tools["Read"] == nil {
			t.Errorf("request #%d does not advertise the Read tool", i+1)
		}
	}
}

func TestAgentLoopRunsUntilModelStopsRequestingTools(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "See app/main.py for the entry point.\n")
	writeFile(t, filepath.Join(dir, "app", "main.py"), "print('entry point')\n")
	readReadme := toolCall{id: "call_1", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": filepath.Join(dir, "README.md")})}
	readMain := toolCall{id: "call_2", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": filepath.Join(dir, "app", "main.py")})}
	llm := newFakeLLM(t,
		assistantToolCalls(readReadme),
		assistantToolCalls(readMain),
		assistantText("It prints 'entry point'."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "What does the entry point do?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "It prints 'entry point'." {
		t.Errorf("stdout = %q, want only the final answer", stdout)
	}

	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	assertMessages(t, requests[2],
		userMessage("What does the entry point do?"),
		assistantMessage("", readReadme),
		toolMessage("call_1", "See app/main.py for the entry point.\n"),
		assistantMessage("", readMain),
		toolMessage("call_2", "print('entry point')\n"),
	)
}

func TestAgentLoopExecutesEveryToolCallOfAResponse(t *testing.T) {
	readA := toolCall{id: "call_a", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": writeTempFile(t, "a.txt", "contents of a")})}
	readB := toolCall{id: "call_b", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": writeTempFile(t, "b.txt", "contents of b")})}
	llm := newFakeLLM(t,
		assistantToolCalls(readA, readB),
		assistantText("done"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "Compare a.txt and b.txt")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "done" {
		t.Errorf("stdout = %q, want %q", stdout, "done")
	}
	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	assertMessages(t, requests[1],
		userMessage("Compare a.txt and b.txt"),
		assistantMessage("", readA, readB),
		toolMessage("call_a", "contents of a"),
		toolMessage("call_b", "contents of b"),
	)
}

func TestAgentLoopCreatesFileWithWriteTool(t *testing.T) {
	const prompt = "Read README.md and create the required file. File should have 1 line. Reply with 'Created the file'"
	const readme = "Create app/main.py containing a hello world program.\n"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), readme)
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	readCall := toolCall{id: "call_1", name: "Read", arguments: `{"file_path": "README.md"}`}
	writeCall := toolCall{id: "call_2", name: "Write", arguments: `{"file_path": "app/main.py", "content": "print('Hello, World!')\n"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(readCall),
		assistantToolCalls(writeCall),
		assistantText("Created the file"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "Created the file" {
		t.Errorf("stdout = %q, want %q", stdout, "Created the file")
	}
	if got := readFile(t, filepath.Join(dir, "app", "main.py")); got != "print('Hello, World!')\n" {
		t.Errorf("app/main.py contains %q, want %q", got, "print('Hello, World!')\n")
	}

	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	assertMessages(t, requests[2],
		userMessage(prompt),
		assistantMessage("", readCall),
		toolMessage("call_1", readme),
		assistantMessage("", writeCall),
		toolMessage("call_2", "Wrote 23 bytes to app/main.py"),
	)
}

func TestAgentLoopDeletesFileWithBashTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", "main.js"), "console.log('main');\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# Current readme\n")
	writeFile(t, filepath.Join(dir, "README_old.md"), "# Old readme\n")
	t.Chdir(dir)
	// LC_ALL=C makes the order of the listing independent of the locale.
	listCall := toolCall{id: "call_1", name: "Bash", arguments: `{"command": "LC_ALL=C ls"}`}
	removeCall := toolCall{id: "call_2", name: "Bash", arguments: `{"command": "rm README_old.md"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(listCall),
		assistantToolCalls(removeCall),
		assistantText("Deleted README_old.md"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "Delete the old readme file.")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "Deleted README_old.md" {
		t.Errorf("stdout = %q, want %q", stdout, "Deleted README_old.md")
	}
	if _, err := os.Stat(filepath.Join(dir, "README_old.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("README_old.md still exists (stat error: %v)", err)
	}
	if got := readFile(t, filepath.Join(dir, "README.md")); got != "# Current readme\n" {
		t.Errorf("README.md contains %q, want it untouched", got)
	}
	if got := readFile(t, filepath.Join(dir, "app", "main.js")); got != "console.log('main');\n" {
		t.Errorf("app/main.js contains %q, want it untouched", got)
	}

	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	assertMessages(t, requests[2],
		userMessage("Delete the old readme file."),
		assistantMessage("", listCall),
		toolMessage("call_1", "README.md\nREADME_old.md\napp\n"),
		assistantMessage("", removeCall),
		toolMessage("call_2", "(no output)"),
	)
}

func TestAgentLoopPrintsOnlyTheFinalAnswer(t *testing.T) {
	const secret = "TOP SECRET FILE CONTENTS\n"
	readCall := toolCall{id: "call_1", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": writeTempFile(t, "secret.txt", secret)})}
	llm := newFakeLLM(t,
		assistantToolCalls(readCall).withContent("I'll read the file first."),
		assistantText("The file holds a secret."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "Describe secret.txt")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "The file holds a secret." {
		t.Errorf("stdout = %q, want only the final answer (no tool results or intermediate messages)", stdout)
	}
	// The intermediate assistant text still belongs to the conversation.
	assertMessages(t, llm.recorded()[1],
		userMessage("Describe secret.txt"),
		assistantMessage("I'll read the file first.", readCall),
		toolMessage("call_1", secret),
	)
}

func TestAgentLoopReportsToolFailuresToTheModel(t *testing.T) {
	tests := []struct {
		name        string
		call        toolCall
		wantContent string
	}{
		{
			name:        "file does not exist",
			call:        toolCall{id: "call_1", name: "Read", arguments: `{"file_path": "/no/such/dir/apple.py"}`},
			wantContent: "no such file",
		},
		{
			name:        "malformed arguments",
			call:        toolCall{id: "call_1", name: "Read", arguments: `{"file_path": `},
			wantContent: "invalid arguments",
		},
		{
			name:        "unknown tool",
			call:        toolCall{id: "call_1", name: "Teleport", arguments: `{}`},
			wantContent: `unknown tool "Teleport"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := newFakeLLM(t,
				assistantToolCalls(tt.call),
				assistantText("I could not do that."),
			)

			stdout, stderr, code := runCLI(t, llm, "-p", "Do the thing")

			if code != 0 {
				t.Fatalf("exit code = %d, want 0: a failed tool call must not abort the run (stderr: %q)", code, stderr)
			}
			if stdout != "I could not do that." {
				t.Errorf("stdout = %q, want the model's final answer", stdout)
			}
			requests := llm.recorded()
			if len(requests) != 2 {
				t.Fatalf("LLM received %d requests, want 2", len(requests))
			}
			messages := requests[1].Body["messages"].([]any)
			result := messages[len(messages)-1].(map[string]any)
			if result["role"] != "tool" || result["tool_call_id"] != "call_1" {
				t.Fatalf("last message = %s, want the result of tool call call_1", toJSON(t, result))
			}
			content, _ := result["content"].(string)
			if !strings.HasPrefix(content, "Error: ") || !strings.Contains(content, tt.wantContent) {
				t.Errorf("tool result = %q, want an error mentioning %q", content, tt.wantContent)
			}
		})
	}
}

func TestAgentLoopStopsWhenTheAPIFails(t *testing.T) {
	readCall := toolCall{id: "call_1", name: "Read", arguments: jsonArgs(t, map[string]any{"file_path": writeTempFile(t, "a.txt", "contents of a")})}
	llm := newFakeLLM(t,
		assistantToolCalls(readCall),
		httpError(http.StatusUnauthorized),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "Read a.txt")

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	if !strings.Contains(stderr, "error:") || !strings.Contains(stderr, "401") {
		t.Errorf("stderr = %q, want an error mentioning the 401 status", stderr)
	}
}

// assertMessages checks that a request carried exactly the given conversation.
func assertMessages(t *testing.T, request llmRequest, want ...map[string]any) {
	t.Helper()
	wantMessages := make([]any, len(want))
	for i, message := range want {
		wantMessages[i] = message
	}
	if got := request.Body["messages"]; !reflect.DeepEqual(got, wantMessages) {
		t.Errorf("messages = %s\nwant %s", toJSON(t, got), toJSON(t, wantMessages))
	}
}
