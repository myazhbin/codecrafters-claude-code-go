package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestRunSendsPromptAndPrintsReply(t *testing.T) {
	llm := newFakeLLM(t, assistantText("Hello there!"))

	stdout, stderr, code := runCLI(t, llm, "-p", "Say hello")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "Hello there!" {
		t.Errorf("stdout = %q, want only the model's reply %q", stdout, "Hello there!")
	}

	request := llm.onlyRequest()
	if request.Method != http.MethodPost || request.Path != "/chat/completions" {
		t.Errorf("request = %s %s, want POST /chat/completions", request.Method, request.Path)
	}
	if request.Authorization != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", request.Authorization, "Bearer test-key")
	}
	if got := request.Body["model"]; got != model {
		t.Errorf("model = %v, want %q", got, model)
	}
	wantMessages := decodeJSON(t, `[{"role": "user", "content": "Say hello"}]`)
	if got := request.Body["messages"]; !reflect.DeepEqual(got, wantMessages) {
		t.Errorf("messages = %s\nwant %s", toJSON(t, got), toJSON(t, wantMessages))
	}
}

func TestRunAdvertisesReadTool(t *testing.T) {
	llm := newFakeLLM(t, assistantText("1"))

	stdout, stderr, code := runCLI(t, llm, "-p", "How many tools are available to you in this request? Number only.")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "1" {
		t.Errorf("stdout = %q, want %q", stdout, "1")
	}

	want := decodeJSON(t, `{
		"type": "function",
		"function": {
			"name": "Read",
			"description": "Read and return the contents of a file",
			"parameters": {
				"type": "object",
				"properties": {
					"file_path": {
						"type": "string",
						"description": "The path to the file to read"
					}
				},
				"required": ["file_path"]
			}
		}
	}`)
	tools := advertisedTools(t, llm.onlyRequest())
	if got := tools["Read"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Read tool = %s\nwant %s", toJSON(t, got), toJSON(t, want))
	}
	if len(tools) != 1 {
		t.Errorf("advertised %d tools, want only Read: %s", len(tools), toJSON(t, tools))
	}
}

func TestRunRejectsInvalidInvocations(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		args       []string
		wantStderr string
	}{
		{
			name:       "missing prompt",
			env:        map[string]string{"OPENROUTER_API_KEY": "test-key"},
			args:       nil,
			wantStderr: "prompt must not be empty",
		},
		{
			name:       "empty prompt",
			env:        map[string]string{"OPENROUTER_API_KEY": "test-key"},
			args:       []string{"-p", ""},
			wantStderr: "prompt must not be empty",
		},
		{
			name:       "missing API key",
			env:        map[string]string{},
			args:       []string{"-p", "hi"},
			wantStderr: "OPENROUTER_API_KEY",
		},
		{
			name:       "unknown flag",
			env:        map[string]string{"OPENROUTER_API_KEY": "test-key"},
			args:       []string{"--no-such-flag"},
			wantStderr: "flag provided but not defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := newFakeLLM(t)
			tt.env["OPENROUTER_BASE_URL"] = llm.server.URL

			stdout, stderr, code := runCLIWithEnv(t, tt.env, tt.args...)

			if code == 0 {
				t.Errorf("exit code = 0, want non-zero")
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tt.wantStderr)
			}
			if requests := llm.recorded(); len(requests) != 0 {
				t.Errorf("LLM received %d requests, want none", len(requests))
			}
		})
	}
}

func TestRunReportsAPIFailure(t *testing.T) {
	llm := newFakeLLM(t, httpError(http.StatusUnauthorized))

	stdout, stderr, code := runCLI(t, llm, "-p", "hi")

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

// advertisedTools returns the tool specifications sent with a request, keyed
// by function name.
func advertisedTools(t *testing.T, request llmRequest) map[string]any {
	t.Helper()
	specs, ok := request.Body["tools"].([]any)
	if !ok {
		t.Fatalf("request has no tools array: %s", toJSON(t, request.Body))
	}
	tools := make(map[string]any, len(specs))
	for _, spec := range specs {
		object, _ := spec.(map[string]any)
		function, _ := object["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			t.Fatalf("tool without a function name: %s", toJSON(t, spec))
		}
		if _, duplicate := tools[name]; duplicate {
			t.Fatalf("tool %q is advertised more than once", name)
		}
		tools[name] = spec
	}
	return tools
}
