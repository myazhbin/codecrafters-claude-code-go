package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestMain makes the tests the ones to decide whether a language server is
// configured, whatever the environment they are run from.
func TestMain(m *testing.M) {
	os.Unsetenv("CODECRAFTERS_LSP_URL")
	os.Exit(m.Run())
}

// fakeLSP stands in for the course's LSP transport: an HTTP endpoint with one
// path per action. It records the calls it receives and answers them with the
// scripted responses.
type fakeLSP struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	responses map[string]any
	calls     []lspCall
	// onCall, if set, is called for every action received.
	onCall func(action string)
}

// lspCall is one call received by the fake LSP transport.
type lspCall struct {
	Action  string
	Payload map[string]any
}

// newFakeLSP starts a fake LSP transport and points the environment of the
// test at it. Until told otherwise, it only answers the configuration request
// that the supplied client makes when it connects.
func newFakeLSP(t *testing.T) *fakeLSP {
	t.Helper()
	lsp := &fakeLSP{t: t, responses: map[string]any{
		"configuration": map[string]any{
			"extensionToLanguage": map[string]any{".py": "python"},
		},
	}}
	lsp.server = httptest.NewServer(http.HandlerFunc(lsp.handle))
	t.Cleanup(lsp.server.Close)
	t.Setenv("CODECRAFTERS_LSP_URL", lsp.server.URL)
	return lsp
}

func (f *fakeLSP) handle(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/")
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("fakeLSP: reading the body of %s: %v", action, err)
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		f.t.Errorf("fakeLSP: the body of %s is not a JSON object: %v\n%s", action, err, raw)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.calls = append(f.calls, lspCall{Action: action, Payload: payload})
	response, known := f.responses[action]
	onCall := f.onCall
	f.mu.Unlock()
	if onCall != nil {
		onCall(action)
	}

	if !known {
		http.Error(w, "fakeLSP: no response scripted for "+action, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// received returns the calls received so far.
func (f *fakeLSP) received() []lspCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lspCall(nil), f.calls...)
}

// lspToolSpec is the specification of the LSP tool that every request must
// advertise while a language server is connected.
const lspToolSpec = `{
	"type": "function",
	"function": {
		"name": "LSP",
		"description": "Returns the file, line, and character where a symbol is defined.",
		"parameters": {
			"type": "object",
			"properties": {
				"operation": {
					"type": "string",
					"enum": ["goToDefinition"]
				},
				"filePath": {
					"type": "string",
					"description": "Absolute path, or a path relative to the agent's current working directory."
				},
				"line": {
					"type": "integer",
					"minimum": 1,
					"description": "1-based line number of the symbol."
				},
				"character": {
					"type": "integer",
					"minimum": 1,
					"description": "1-based character offset in UTF-16 code units (same as character count for ASCII)."
				}
			},
			"required": ["operation", "filePath", "line", "character"]
		}
	}
}`

func TestRunConnectsLSPClientBeforeFirstModelRequest(t *testing.T) {
	lsp := newFakeLSP(t)
	llm := newFakeLLM(t, assistantText("ready"))
	modelRequestsBeforeConnecting := -1
	lsp.onCall = func(action string) {
		if action == "configuration" {
			modelRequestsBeforeConnecting = len(llm.recorded())
		}
	}

	stdout, stderr, code := runCLI(t, llm, "-p", "Are you ready?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if modelRequestsBeforeConnecting != 0 {
		t.Errorf("the LSP client connected after %d model requests, want it connected before the first one", modelRequestsBeforeConnecting)
	}
	if calls := lsp.received(); len(calls) != 1 || calls[0].Action != "configuration" {
		t.Errorf("LSP transport received %+v, want a single configuration request", calls)
	}
	// The startup log goes to stderr: stdout is reserved for the answer.
	if !strings.Contains(stderr, "LSP client connected\n") {
		t.Errorf("stderr = %q, want it to report that the LSP client connected", stderr)
	}
	if stdout != "ready" {
		t.Errorf("stdout = %q, want only the model's answer %q", stdout, "ready")
	}
}

func TestRunAdvertisesLSPToolWithEveryRequestWhenConnected(t *testing.T) {
	const prompt = "Use LSP goToDefinition on checkout.py at line 5, character 12. " +
		"If navigation is not implemented yet, report the tool response without retrying."
	newFakeLSP(t)
	goToDefinition := toolCall{
		id:        "call_lsp_1",
		name:      "LSP",
		arguments: `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`,
	}
	llm := newFakeLLM(t,
		assistantToolCalls(goToDefinition),
		assistantText("The tool responded: LSP is not implemented yet"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "The tool responded: LSP is not implemented yet" {
		t.Errorf("stdout = %q, want the model's report", stdout)
	}
	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	want := decodeJSON(t, lspToolSpec)
	for i, request := range requests {
		tools := advertisedTools(t, request)
		if got := tools["LSP"]; !reflect.DeepEqual(got, want) {
			t.Errorf("request #%d: LSP tool = %s\nwant %s", i+1, toJSON(t, got), toJSON(t, want))
		}
		for _, name := range []string{"Read", "Write", "Bash"} {
			if tools[name] == nil {
				t.Errorf("request #%d no longer advertises the %s tool", i+1, name)
			}
		}
	}
	// The call gets a tool result, with the matching tool_call_id.
	assertMessages(t, requests[1],
		userMessage(prompt),
		assistantMessage("", goToDefinition),
		toolMessage("call_lsp_1", "LSP is not implemented yet"),
	)
}

func TestRunWithoutLSPConfigurationKeepsItsOtherTools(t *testing.T) {
	llm := newFakeLLM(t, assistantText("3"))

	_, stderr, code := runCLI(t, llm, "-p", "How many tools do you have?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	tools := advertisedTools(t, llm.onlyRequest())
	if tools["LSP"] != nil {
		t.Errorf("the LSP tool is advertised without a language server")
	}
	for _, name := range []string{"Read", "Write", "Bash"} {
		if tools[name] == nil {
			t.Errorf("the %s tool is not advertised", name)
		}
	}
	if strings.Contains(stderr, "LSP") {
		t.Errorf("stderr = %q, want no mention of LSP when it is not configured", stderr)
	}
}

func TestRunCarriesOnWithoutLSPWhenItCannotConnect(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"nothing listens at the configured address", "http://" + unusedAddress(t)},
		{"configured address is empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CODECRAFTERS_LSP_URL", tt.url)
			llm := newFakeLLM(t, assistantText("ok"))

			stdout, stderr, code := runCLI(t, llm, "-p", "hi")

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
			if stdout != "ok" {
				t.Errorf("stdout = %q, want %q", stdout, "ok")
			}
			if !strings.Contains(stderr, "warning: LSP is unavailable") {
				t.Errorf("stderr = %q, want a warning that LSP is unavailable", stderr)
			}
			if strings.Contains(stderr, "LSP client connected") {
				t.Errorf("stderr = %q, want no claim that the LSP client connected", stderr)
			}
			if advertisedTools(t, llm.onlyRequest())["LSP"] != nil {
				t.Errorf("the LSP tool is advertised without a language server")
			}
		})
	}
}

func TestRunOffersLSPToolAlongsideSkillsAndToSubagents(t *testing.T) {
	newFakeLSP(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, skillsDir, "apple", skillFile), lines(
		"---",
		"name: apple",
		"description: Use this skill when the user asks where a symbol is defined.",
		"context: fork",
		"---",
		"",
		"Find the definition with the LSP tool.",
	))
	t.Chdir(project)
	llm := newFakeLLM(t,
		assistantToolCalls(toolCall{id: "call_1", name: "Skill", arguments: `{"name": "apple"}`}), // main agent
		assistantText("pricing.py"),                   // subagent
		assistantText("It is defined in pricing.py."), // main agent
	)

	_, stderr, code := runCLI(t, llm, "-p", "Where is invoice_total defined?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	wantTools := [][]string{
		{"Read", "Write", "Bash", "LSP", "Skill"}, // main agent
		{"Read", "Write", "Bash", "LSP"},          // subagent
		{"Read", "Write", "Bash", "LSP", "Skill"}, // main agent
	}
	for i, request := range requests {
		tools := advertisedTools(t, request)
		for _, name := range wantTools[i] {
			if tools[name] == nil {
				t.Errorf("request #%d does not advertise the %s tool", i+1, name)
			}
		}
		if len(tools) != len(wantTools[i]) {
			t.Errorf("request #%d advertises %d tools, want %q", i+1, len(tools), wantTools[i])
		}
	}
}

// unusedAddress returns a local address that nothing listens on.
func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}
