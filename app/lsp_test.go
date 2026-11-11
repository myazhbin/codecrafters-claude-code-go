package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// fakeLSP stands in for the course's LSP transport and the language server
// behind it: an HTTP endpoint with one path per action of the supplied client.
// It records the calls it receives, answers requests with the scripted
// results and, like the real server, keeps the documents it is sent.
type fakeLSP struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// results holds the scripted result of the requests, by method: a value,
	// or a func(text string) any that makes the result out of the server's
	// copy of the document that the request is about.
	results map[string]any
	// failures holds how many of the next messages of a method must fail.
	failures map[string]int
	// documents holds the server's copy of the documents it was sent, by URI.
	documents map[string]fakeDocument
	calls     []lspCall
	// onCall, if set, is called for every action received.
	onCall func(action string)
}

// fakeDocument is the fake server's copy of a document.
type fakeDocument struct {
	Version int
	Text    string
}

// lspCall is one call received by the fake LSP transport.
type lspCall struct {
	Action string // configuration, notify or request
	Method string // the LSP method of a notification or of a request
	Params map[string]any
}

// newFakeLSP starts a fake LSP transport, for a server that handles Python,
// and points the environment of the test at it.
func newFakeLSP(t *testing.T) *fakeLSP {
	t.Helper()
	lsp := &fakeLSP{
		t:         t,
		results:   map[string]any{},
		failures:  map[string]int{},
		documents: map[string]fakeDocument{},
	}
	lsp.server = httptest.NewServer(http.HandlerFunc(lsp.handle))
	t.Cleanup(lsp.server.Close)
	t.Setenv("CODECRAFTERS_LSP_URL", lsp.server.URL)
	return lsp
}

// script sets the result of the requests of a method.
func (f *fakeLSP) script(method string, result any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[method] = result
}

// failNext makes the next message of a method fail.
func (f *fakeLSP) failNext(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method]++
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
	call := lspCall{Action: action}
	call.Method, _ = payload["method"].(string)
	call.Params, _ = payload["params"].(map[string]any)

	f.mu.Lock()
	f.calls = append(f.calls, call)
	status, response := f.answer(call)
	onCall := f.onCall
	f.mu.Unlock()
	if onCall != nil {
		onCall(action)
	}

	if status != http.StatusOK {
		http.Error(w, response.(string), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// answer returns the HTTP status and the body that respond to a call. The
// body of a failure is its explanation.
func (f *fakeLSP) answer(call lspCall) (status int, response any) {
	if call.Action == "configuration" {
		return http.StatusOK, map[string]any{
			"extensionToLanguage": map[string]any{".py": "python"},
		}
	}
	if f.failures[call.Method] > 0 {
		f.failures[call.Method]--
		return http.StatusInternalServerError, "scripted failure of " + call.Method
	}
	textDocument, _ := call.Params["textDocument"].(map[string]any)
	uri, _ := textDocument["uri"].(string)
	version, _ := textDocument["version"].(float64)
	document, open := f.documents[uri]
	switch {
	case call.Action == "notify" && call.Method == "textDocument/didOpen":
		if open {
			return http.StatusBadRequest, "document is already open: " + uri
		}
		text, _ := textDocument["text"].(string)
		f.documents[uri] = fakeDocument{Version: int(version), Text: text}
		return http.StatusOK, map[string]any{}
	case call.Action == "notify" && call.Method == "textDocument/didChange":
		if !open {
			return http.StatusBadRequest, "document is not open: " + uri
		}
		if int(version) <= document.Version {
			return http.StatusBadRequest, "version does not go up: " + uri
		}
		changes, _ := call.Params["contentChanges"].([]any)
		if len(changes) != 1 {
			return http.StatusBadRequest, "want the full text as the one content change: " + uri
		}
		change, _ := changes[0].(map[string]any)
		text, _ := change["text"].(string)
		f.documents[uri] = fakeDocument{Version: int(version), Text: text}
		return http.StatusOK, map[string]any{}
	case call.Action == "request":
		if !open {
			return http.StatusBadRequest, "document is not open: " + uri
		}
		result, scripted := f.results[call.Method]
		if !scripted {
			return http.StatusNotFound, "no result scripted for " + call.Method
		}
		if resultFor, dynamic := result.(func(text string) any); dynamic {
			result = resultFor(document.Text)
		}
		return http.StatusOK, result
	default:
		return http.StatusNotFound, "unexpected " + call.Action + " " + call.Method
	}
}

// copyOf returns the server's copy of the file at path, if it was sent it.
func (f *fakeLSP) copyOf(path string) (document fakeDocument, open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	document, open = f.documents[fileURI(path)]
	return document, open
}

// received returns the calls received so far.
func (f *fakeLSP) received() []lspCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lspCall(nil), f.calls...)
}

// exchanges summarizes the calls received so far: the method of each message,
// or the action of a call that carries no message.
func (f *fakeLSP) exchanges() []string {
	var exchanges []string
	for _, call := range f.received() {
		if call.Method != "" {
			exchanges = append(exchanges, call.Method)
		} else {
			exchanges = append(exchanges, call.Action)
		}
	}
	return exchanges
}

// locationOf builds the location of a range of a file as the language server
// reports it, with positions counted from zero.
func locationOf(path string, startLine, startCharacter, endLine, endCharacter int) map[string]any {
	return map[string]any{
		"uri": fileURI(path),
		"range": map[string]any{
			"start": map[string]any{"line": startLine, "character": startCharacter},
			"end":   map[string]any{"line": endLine, "character": endCharacter},
		},
	}
}

// The invoice project: checkout.py calls pricing.total under another name and
// reports.py under its own, while discounts.py and preview.py have unrelated
// functions of the same name.
const (
	checkoutSource = "from pricing import total as invoice_total\n" +
		"\n" +
		"\n" +
		"def checkout(items):\n" +
		"    return invoice_total(items)\n"
	pricingSource = "def total(items):\n" +
		"    return sum(item.price * item.quantity for item in items)\n"
	reportsSource = "from pricing import total\n" +
		"\n" +
		"\n" +
		"def report(orders, refunds):\n" +
		"    return total(orders) - total(refunds)\n"
	discountsSource = "def total(items):\n" +
		"    return 0\n"
	previewSource = "def total(lines):\n" +
		"    return len(lines)\n"

	// discountedCheckoutSource is checkout.py once it imports total from
	// discounts rather than from pricing.
	discountedCheckoutSource = "from discounts import total as invoice_total\n" +
		"\n" +
		"\n" +
		"def checkout(items):\n" +
		"    return invoice_total(items)\n"
)

// newInvoiceProject creates the invoice project in a new directory, makes it
// the working directory and returns its path.
func newInvoiceProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "checkout.py"), checkoutSource)
	writeFile(t, filepath.Join(project, "pricing.py"), pricingSource)
	writeFile(t, filepath.Join(project, "reports.py"), reportsSource)
	writeFile(t, filepath.Join(project, "discounts.py"), discountsSource)
	writeFile(t, filepath.Join(project, "preview.py"), previewSource)
	t.Chdir(project)
	return project
}

// newConnectedLSPTool returns the LSP tool, connected to the fake transport.
func newConnectedLSPTool(t *testing.T) Tool {
	t.Helper()
	server := connectLSP(io.Discard)
	if server == nil {
		t.Fatal("cannot connect to the fake LSP transport")
	}
	return newLSPTool(server)
}

// newConnectedToolbox returns the tools of an agent that is connected to the
// fake LSP transport.
func newConnectedToolbox(t *testing.T) Toolbox {
	t.Helper()
	server := connectLSP(io.Discard)
	if server == nil {
		t.Fatal("cannot connect to the fake LSP transport")
	}
	return baseToolbox(server)
}

// followCheckoutImport scripts where invoice_total, in checkout.py, is
// defined: in the module that the server's copy of checkout.py imports it
// from.
func followCheckoutImport(lsp *fakeLSP, project string) {
	lsp.script("textDocument/definition", func(text string) any {
		module := "pricing.py"
		if strings.HasPrefix(text, "from discounts import") {
			module = "discounts.py"
		}
		return locationOf(filepath.Join(project, module), 0, 4, 0, 9)
	})
}

// notifications summarizes the notifications that the fake server received
// about documents, as in "textDocument/didChange checkout.py v2".
func (f *fakeLSP) notifications() []string {
	var notifications []string
	for _, call := range f.received() {
		if call.Action != "notify" {
			continue
		}
		textDocument, _ := call.Params["textDocument"].(map[string]any)
		uri, _ := textDocument["uri"].(string)
		version, _ := textDocument["version"].(float64)
		notifications = append(notifications, fmt.Sprintf("%s %s v%d", call.Method, filepath.Base(pathFromURI(uri)), int(version)))
	}
	return notifications
}

// lspToolSpec is the specification of the LSP tool that every request must
// advertise while a language server is connected.
const lspToolSpec = `{
	"type": "function",
	"function": {
		"name": "LSP",
		"description": "Asks the language server about the symbol at a position of a file. goToDefinition returns the file, line, and character where the symbol is defined. findReferences returns the file, line, and character of every reference to the symbol, such as the calls of a function, including through aliases. Save your edits with the Write tool: the language server then answers about the new code.",
		"parameters": {
			"type": "object",
			"properties": {
				"operation": {
					"type": "string",
					"enum": ["goToDefinition", "findReferences"]
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
	if got, want := lsp.exchanges(), []string{"configuration"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LSP transport received %q, want %q", got, want)
	}
	// The startup log goes to stderr: stdout is reserved for the answer.
	if !strings.Contains(stderr, "LSP client connected\n") {
		t.Errorf("stderr = %q, want it to report that the LSP client connected", stderr)
	}
	if stdout != "ready" {
		t.Errorf("stdout = %q, want only the model's answer %q", stdout, "ready")
	}
}

func TestRunFindsDefinitionWithLSP(t *testing.T) {
	const prompt = "Use LSP goToDefinition on checkout.py at line 5, character 12. " +
		"Read the returned implementation and explain which total is called; preview.total is unrelated."
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	// The name "total" in "def total(items):".
	lsp.script("textDocument/definition", locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9))
	goToDefinition := toolCall{
		id:        "call_lsp_1",
		name:      "LSP",
		arguments: `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`,
	}
	readImplementation := toolCall{id: "call_read_1", name: "Read", arguments: `{"file_path": "pricing.py"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(goToDefinition),
		assistantToolCalls(readImplementation),
		assistantText("checkout calls pricing.total."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "checkout calls pricing.total." {
		t.Errorf("stdout = %q, want the model's explanation", stdout)
	}

	// The server gets the document, then the request about it, with the
	// position counted from zero.
	checkoutURI := fileURI(filepath.Join(project, "checkout.py"))
	wantCalls := []lspCall{
		{Action: "configuration"},
		{Action: "notify", Method: "textDocument/didOpen", Params: map[string]any{
			"textDocument": map[string]any{
				"uri":        checkoutURI,
				"languageId": "python",
				"version":    float64(1),
				"text":       checkoutSource,
			},
		}},
		{Action: "request", Method: "textDocument/definition", Params: map[string]any{
			"textDocument": map[string]any{"uri": checkoutURI},
			"position":     map[string]any{"line": float64(4), "character": float64(11)},
		}},
	}
	if got := lsp.received(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("LSP transport received %s\nwant %s", toJSON(t, got), toJSON(t, wantCalls))
	}

	// The model gets the definition, relative to the working directory and
	// counted from one, and reads the implementation with it.
	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	assertMessages(t, requests[2],
		userMessage(prompt),
		assistantMessage("", goToDefinition),
		toolMessage("call_lsp_1", "Defined in pricing.py:1:5"),
		assistantMessage("", readImplementation),
		toolMessage("call_read_1", pricingSource),
	)
	wantSpec := decodeJSON(t, lspToolSpec)
	for i, request := range requests {
		tools := advertisedTools(t, request)
		if got := tools["LSP"]; !reflect.DeepEqual(got, wantSpec) {
			t.Errorf("request #%d: LSP tool = %s\nwant %s", i+1, toJSON(t, got), toJSON(t, wantSpec))
		}
		for _, name := range []string{"Read", "Write", "Bash"} {
			if tools[name] == nil {
				t.Errorf("request #%d no longer advertises the %s tool", i+1, name)
			}
		}
	}
}

func TestLSPToolOpensEachDocumentOnce(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.script("textDocument/definition", locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9))
	tool := newConnectedLSPTool(t)

	// The same document, however its path is written, then another one.
	for _, arguments := range []string{
		`{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`,
		`{"operation": "goToDefinition", "filePath": "./checkout.py", "line": 1, "character": 30}`,
		jsonArgs(t, map[string]any{"operation": "goToDefinition", "filePath": filepath.Join(project, "checkout.py"), "line": 5, "character": 12}),
		`{"operation": "goToDefinition", "filePath": "preview.py", "line": 1, "character": 5}`,
	} {
		// The fake server rejects a document that is opened twice.
		got, err := tool.Run(context.Background(), arguments)
		if err != nil {
			t.Fatalf("LSP(%s) returned an error: %v", arguments, err)
		}
		if got != "Defined in pricing.py:1:5" {
			t.Errorf("LSP(%s) = %q, want %q", arguments, got, "Defined in pricing.py:1:5")
		}
	}

	want := []string{
		"configuration",
		"textDocument/didOpen", "textDocument/definition",
		"textDocument/definition",
		"textDocument/definition",
		"textDocument/didOpen", "textDocument/definition",
	}
	if got := lsp.exchanges(); !reflect.DeepEqual(got, want) {
		t.Errorf("LSP transport received %q\nwant %q", got, want)
	}
}

func TestLSPToolOpensDocumentAgainAfterFailedNotification(t *testing.T) {
	const arguments = `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.script("textDocument/definition", locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9))
	lsp.failNext("textDocument/didOpen")
	tool := newConnectedLSPTool(t)

	got, err := tool.Run(context.Background(), arguments)

	if err == nil || !strings.Contains(err.Error(), "scripted failure of textDocument/didOpen") {
		t.Fatalf("LSP = %q, %v; want the failure of the notification reported", got, err)
	}
	// The document did not reach the server: no request is made about it...
	if got, want := lsp.exchanges(), []string{"configuration", "textDocument/didOpen"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LSP transport received %q, want %q", got, want)
	}

	// ...and the next lookup sends it again.
	got, err = tool.Run(context.Background(), arguments)

	if err != nil {
		t.Fatalf("LSP returned an error: %v", err)
	}
	if got != "Defined in pricing.py:1:5" {
		t.Errorf("LSP = %q, want %q", got, "Defined in pricing.py:1:5")
	}
	want := []string{"configuration", "textDocument/didOpen", "textDocument/didOpen", "textDocument/definition"}
	if got := lsp.exchanges(); !reflect.DeepEqual(got, want) {
		t.Errorf("LSP transport received %q\nwant %q", got, want)
	}
}

func TestLSPToolIdentifiesDocumentsByEncodedAbsoluteURI(t *testing.T) {
	project := newInvoiceProject(t)
	writeFile(t, filepath.Join(project, "billing", "cart notes.py"), "from pricing import total\n")
	lsp := newFakeLSP(t)
	lsp.script("textDocument/definition", locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9))
	tool := newConnectedLSPTool(t)

	_, err := tool.Run(context.Background(), `{"operation": "goToDefinition", "filePath": "billing/cart notes.py", "line": 1, "character": 21}`)

	if err != nil {
		t.Fatalf("LSP returned an error: %v", err)
	}
	wantURI := "file://" + filepath.ToSlash(project) + "/billing/cart%20notes.py"
	calls := lsp.received()
	if len(calls) != 3 {
		t.Fatalf("LSP transport received %d calls, want 3", len(calls))
	}
	for _, call := range calls[1:] {
		textDocument, _ := call.Params["textDocument"].(map[string]any)
		if textDocument["uri"] != wantURI {
			t.Errorf("%s identifies the document as %q, want %q", call.Method, textDocument["uri"], wantURI)
		}
	}
}

func TestLSPToolDescribesDefinitions(t *testing.T) {
	project := newInvoiceProject(t)
	writeFile(t, filepath.Join(project, "billing", "cart notes.py"), "def total(cart):\n    return 0\n")
	// The server may know the project through other symbolic links than the
	// agent does, as with macOS temporary directories.
	resolvedProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	outside := writeTempFile(t, "library.py", "def total():\n    pass\n")
	tests := []struct {
		name   string
		result any
		want   string
	}{
		{
			name:   "file of the working directory",
			result: locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9),
			want:   "Defined in pricing.py:1:5",
		},
		{
			name:   "position counted from one",
			result: locationOf(filepath.Join(project, "pricing.py"), 41, 0, 41, 9),
			want:   "Defined in pricing.py:42:1",
		},
		{
			name:   "file in a subdirectory, with an encoded URI",
			result: locationOf(filepath.Join(project, "billing", "cart notes.py"), 0, 4, 0, 9),
			want:   "Defined in " + filepath.Join("billing", "cart notes.py") + ":1:5",
		},
		{
			name:   "file known to the server through other symbolic links",
			result: locationOf(filepath.Join(resolvedProject, "pricing.py"), 0, 4, 0, 9),
			want:   "Defined in pricing.py:1:5",
		},
		{
			name:   "file outside the working directory",
			result: locationOf(outside, 0, 4, 0, 9),
			want:   "Defined in " + outside + ":1:5",
		},
		{
			name: "several definitions",
			result: []any{
				locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9),
				locationOf(filepath.Join(project, "preview.py"), 0, 4, 0, 9),
			},
			want: "Defined in pricing.py:1:5\nDefined in preview.py:1:5",
		},
		{
			name:   "no definition",
			result: nil,
			want:   "No definition found",
		},
		{
			name:   "empty list of definitions",
			result: []any{},
			want:   "No definition found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lsp := newFakeLSP(t)
			lsp.script("textDocument/definition", tt.result)
			tool := newConnectedLSPTool(t)

			got, err := tool.Run(context.Background(), `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`)

			if err != nil {
				t.Fatalf("LSP returned an error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LSP = %q\nwant %q", got, tt.want)
			}
		})
	}
}

// invoiceTotalReferences returns what the server knows to refer to
// pricing.total in the invoice project: its declaration, the imports and the
// calls, whatever the name they use.
func invoiceTotalReferences(project string) []any {
	return []any{
		locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9),
		locationOf(filepath.Join(project, "checkout.py"), 0, 20, 0, 25),
		locationOf(filepath.Join(project, "checkout.py"), 4, 11, 4, 24),
		locationOf(filepath.Join(project, "reports.py"), 0, 20, 0, 25),
		locationOf(filepath.Join(project, "reports.py"), 4, 11, 4, 16),
		locationOf(filepath.Join(project, "reports.py"), 4, 27, 4, 32),
	}
}

func TestRunFindsReferencesWithLSP(t *testing.T) {
	const prompt = "Use LSP findReferences on pricing.py at line 1, character 5, and list the callers " +
		"that a change to pricing.total's return type would affect. Don't change any code. " +
		"Preserve the returned locations; preview.total and comments are unrelated."
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.script("textDocument/references", invoiceTotalReferences(project))
	findReferences := toolCall{
		id:        "call_lsp_1",
		name:      "LSP",
		arguments: `{"operation": "findReferences", "filePath": "pricing.py", "line": 1, "character": 5}`,
	}
	llm := newFakeLLM(t,
		assistantToolCalls(findReferences),
		assistantText("checkout.py and reports.py are affected."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "checkout.py and reports.py are affected." {
		t.Errorf("stdout = %q, want the model's list of callers", stdout)
	}

	// The server gets the queried document, then the request about the symbol
	// at the zero-based position, declaration included.
	pricingURI := fileURI(filepath.Join(project, "pricing.py"))
	wantCalls := []lspCall{
		{Action: "configuration"},
		{Action: "notify", Method: "textDocument/didOpen", Params: map[string]any{
			"textDocument": map[string]any{
				"uri":        pricingURI,
				"languageId": "python",
				"version":    float64(1),
				"text":       pricingSource,
			},
		}},
		{Action: "request", Method: "textDocument/references", Params: map[string]any{
			"textDocument": map[string]any{"uri": pricingURI},
			"position":     map[string]any{"line": float64(0), "character": float64(4)},
			"context":      map[string]any{"includeDeclaration": true},
		}},
	}
	if got := lsp.received(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("LSP transport received %s\nwant %s", toJSON(t, got), toJSON(t, wantCalls))
	}

	// Every location reaches the model, with its file and one-based position.
	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	assertMessages(t, requests[1],
		userMessage(prompt),
		assistantMessage("", findReferences),
		toolMessage("call_lsp_1", strings.Join([]string{
			"Found 6 references across 3 files:",
			"",
			"pricing.py:",
			"  Line 1:5",
			"",
			"checkout.py:",
			"  Line 1:21",
			"  Line 5:12",
			"",
			"reports.py:",
			"  Line 1:21",
			"  Line 5:12",
			"  Line 5:28",
		}, "\n")),
	)
	wantSpec := decodeJSON(t, lspToolSpec)
	for i, request := range requests {
		if got := advertisedTools(t, request)["LSP"]; !reflect.DeepEqual(got, wantSpec) {
			t.Errorf("request #%d: LSP tool = %s\nwant %s", i+1, toJSON(t, got), toJSON(t, wantSpec))
		}
	}
}

func TestRunReportsThatNoReferencesWereFound(t *testing.T) {
	newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.script("textDocument/references", []any{})
	findReferences := toolCall{
		id:        "call_lsp_1",
		name:      "LSP",
		arguments: `{"operation": "findReferences", "filePath": "pricing.py", "line": 1, "character": 5}`,
	}
	llm := newFakeLLM(t,
		assistantToolCalls(findReferences),
		assistantText("Nothing calls it."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "Who calls pricing.total?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "Nothing calls it." {
		t.Errorf("stdout = %q, want the model's answer", stdout)
	}
	assertMessages(t, llm.recorded()[1],
		userMessage("Who calls pricing.total?"),
		assistantMessage("", findReferences),
		toolMessage("call_lsp_1", "No references found"),
	)
}

func TestLSPToolOpensTheDocumentItIsAskedAbout(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.script("textDocument/definition", locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9))
	lsp.script("textDocument/references", invoiceTotalReferences(project))
	tool := newConnectedLSPTool(t)

	// Looking a definition up from checkout.py does not send pricing.py to
	// the server, even though that is where the definition is.
	for _, arguments := range []string{
		`{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`,
		`{"operation": "findReferences", "filePath": "pricing.py", "line": 1, "character": 5}`,
		`{"operation": "findReferences", "filePath": "checkout.py", "line": 5, "character": 12}`,
	} {
		if _, err := tool.Run(context.Background(), arguments); err != nil {
			t.Fatalf("LSP(%s) returned an error: %v", arguments, err)
		}
	}

	var got []string
	for _, call := range lsp.received()[1:] {
		textDocument, _ := call.Params["textDocument"].(map[string]any)
		uri, _ := textDocument["uri"].(string)
		got = append(got, call.Method+" "+filepath.Base(pathFromURI(uri)))
	}
	want := []string{
		"textDocument/didOpen checkout.py",
		"textDocument/definition checkout.py",
		"textDocument/didOpen pricing.py",
		"textDocument/references pricing.py",
		"textDocument/references checkout.py",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LSP transport received %q\nwant %q", got, want)
	}
}

func TestLSPToolDescribesReferences(t *testing.T) {
	project := newInvoiceProject(t)
	outside := writeTempFile(t, "library.py", "from pricing import total\n")
	tests := []struct {
		name   string
		result any
		want   string
	}{
		{
			name:   "one reference",
			result: []any{locationOf(filepath.Join(project, "pricing.py"), 0, 4, 0, 9)},
			want: "Found 1 reference across 1 file:\n" +
				"\n" +
				"pricing.py:\n" +
				"  Line 1:5",
		},
		{
			name: "several references in one file stay separate",
			result: []any{
				locationOf(filepath.Join(project, "reports.py"), 4, 11, 4, 16),
				locationOf(filepath.Join(project, "reports.py"), 4, 27, 4, 32),
			},
			want: "Found 2 references across 1 file:\n" +
				"\n" +
				"reports.py:\n" +
				"  Line 5:12\n" +
				"  Line 5:28",
		},
		{
			name: "files in the order of the server, each listed once",
			result: []any{
				locationOf(filepath.Join(project, "reports.py"), 4, 11, 4, 16),
				locationOf(filepath.Join(project, "billing", "tax.py"), 9, 0, 9, 5),
				locationOf(filepath.Join(project, "reports.py"), 0, 20, 0, 25),
				locationOf(outside, 0, 20, 0, 25),
			},
			want: "Found 4 references across 3 files:\n" +
				"\n" +
				"reports.py:\n" +
				"  Line 5:12\n" +
				"  Line 1:21\n" +
				"\n" +
				filepath.Join("billing", "tax.py") + ":\n" +
				"  Line 10:1\n" +
				"\n" +
				outside + ":\n" +
				"  Line 1:21",
		},
		{
			name:   "no references",
			result: []any{},
			want:   "No references found",
		},
		{
			name:   "no answer",
			result: nil,
			want:   "No references found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lsp := newFakeLSP(t)
			lsp.script("textDocument/references", tt.result)
			tool := newConnectedLSPTool(t)

			got, err := tool.Run(context.Background(), `{"operation": "findReferences", "filePath": "pricing.py", "line": 1, "character": 5}`)

			if err != nil {
				t.Fatalf("LSP returned an error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LSP = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestLSPToolRejectsBadCalls(t *testing.T) {
	project := newInvoiceProject(t)
	writeFile(t, filepath.Join(project, "notes.txt"), "not code\n")
	tests := []struct {
		name          string
		arguments     string
		wantErr       string
		wantExchanges []string
	}{
		{
			name:      "malformed JSON",
			arguments: `{"operation": `,
			wantErr:   "invalid arguments",
		},
		{
			name:      "wrong argument type",
			arguments: `{"operation": "goToDefinition", "filePath": "checkout.py", "line": "five", "character": 12}`,
			wantErr:   "invalid arguments",
		},
		{
			name:      "unsupported operation",
			arguments: `{"operation": "rename", "filePath": "checkout.py", "line": 5, "character": 12}`,
			wantErr:   `unsupported operation "rename"`,
		},
		{
			name:      "missing operation",
			arguments: `{"filePath": "checkout.py", "line": 5, "character": 12}`,
			wantErr:   `unsupported operation ""`,
		},
		{
			name:      "missing filePath",
			arguments: `{"operation": "goToDefinition", "line": 5, "character": 12}`,
			wantErr:   "filePath is required",
		},
		{
			name:      "line counted from zero",
			arguments: `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 0, "character": 12}`,
			wantErr:   "line and character are counted from 1",
		},
		{
			name:      "missing character",
			arguments: `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5}`,
			wantErr:   "line and character are counted from 1",
		},
		{
			name:      "file does not exist",
			arguments: `{"operation": "goToDefinition", "filePath": "missing.py", "line": 5, "character": 12}`,
			wantErr:   "no such file",
		},
		{
			name:      "file of a language that the server does not handle",
			arguments: `{"operation": "goToDefinition", "filePath": "notes.txt", "line": 1, "character": 1}`,
			wantErr:   "no language server is configured for files like notes.txt",
		},
		{
			name:          "definition request refused by the server",
			arguments:     `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`,
			wantErr:       "scripted failure of textDocument/definition",
			wantExchanges: []string{"textDocument/didOpen", "textDocument/definition"},
		},
		{
			name:          "references request refused by the server",
			arguments:     `{"operation": "findReferences", "filePath": "pricing.py", "line": 1, "character": 5}`,
			wantErr:       "scripted failure of textDocument/references",
			wantExchanges: []string{"textDocument/didOpen", "textDocument/references"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lsp := newFakeLSP(t)
			lsp.failNext("textDocument/definition")
			lsp.failNext("textDocument/references")
			tool := newConnectedLSPTool(t)

			got, err := tool.Run(context.Background(), tt.arguments)

			if err == nil {
				t.Fatalf("LSP = %q, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
			// A call that is rejected up front does not reach the server.
			wantExchanges := append([]string{"configuration"}, tt.wantExchanges...)
			if got := lsp.exchanges(); !reflect.DeepEqual(got, wantExchanges) {
				t.Errorf("LSP transport received %q, want %q", got, wantExchanges)
			}
		})
	}
}

func TestFileURI(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/project/checkout.py", "file:///project/checkout.py"},
		{"/project/cart notes.py", "file:///project/cart%20notes.py"},
		{"/project/100% née #1?.py", "file:///project/100%25%20n%C3%A9e%20%231%3F.py"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			uri := fileURI(tt.path)

			if uri != tt.want {
				t.Errorf("fileURI(%q) = %q, want %q", tt.path, uri, tt.want)
			}
			if path := pathFromURI(uri); path != tt.path {
				t.Errorf("pathFromURI(%q) = %q, want %q back", uri, path, tt.path)
			}
		})
	}
}

func TestPathFromURILeavesOtherURIsAlone(t *testing.T) {
	for _, uri := range []string{"untitled:Untitled-1", "https://example.com/pricing.py", "::not a uri::"} {
		if got := pathFromURI(uri); got != uri {
			t.Errorf("pathFromURI(%q) = %q, want it unchanged", uri, got)
		}
	}
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
