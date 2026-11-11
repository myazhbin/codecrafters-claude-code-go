package main

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const goToCheckoutTotal = `{"operation": "goToDefinition", "filePath": "checkout.py", "line": 5, "character": 12}`

// wroteCheckout is what the Write tool reports once it has saved checkout.py
// with the given content.
func wroteCheckout(content string) string {
	return fmt.Sprintf("Wrote %d bytes to checkout.py", len(content))
}

func TestRunKeepsLanguageServerCurrentWithWrites(t *testing.T) {
	const prompt = "Use LSP goToDefinition on checkout.py at line 5, character 12. " +
		"Use Write to change its import from pricing to discounts, preserving the invoice_total alias. " +
		"Repeat definition lookup at the same position and report the new target. " +
		"Restore the pricing import using Write and repeat the definition lookup."
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	lookup := func(id string) toolCall {
		return toolCall{id: id, name: "LSP", arguments: goToCheckoutTotal}
	}
	write := func(id, content string) toolCall {
		return toolCall{id: id, name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": content})}
	}
	llm := newFakeLLM(t,
		assistantToolCalls(lookup("call_1")),
		assistantToolCalls(write("call_2", discountedCheckoutSource)),
		assistantToolCalls(lookup("call_3")),
		assistantToolCalls(write("call_4", checkoutSource)),
		assistantToolCalls(lookup("call_5")),
		assistantText("pricing.py, then discounts.py, then pricing.py again."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "pricing.py, then discounts.py, then pricing.py again." {
		t.Errorf("stdout = %q, want the model's report", stdout)
	}

	// Every lookup reflects the code as it was last saved.
	requests := llm.recorded()
	if len(requests) != 6 {
		t.Fatalf("LLM received %d requests, want 6", len(requests))
	}
	assertMessages(t, requests[5],
		userMessage(prompt),
		assistantMessage("", lookup("call_1")),
		toolMessage("call_1", "Defined in pricing.py:1:5"),
		assistantMessage("", write("call_2", discountedCheckoutSource)),
		toolMessage("call_2", wroteCheckout(discountedCheckoutSource)),
		assistantMessage("", lookup("call_3")),
		toolMessage("call_3", "Defined in discounts.py:1:5"),
		assistantMessage("", write("call_4", checkoutSource)),
		toolMessage("call_4", wroteCheckout(checkoutSource)),
		assistantMessage("", lookup("call_5")),
		toolMessage("call_5", "Defined in pricing.py:1:5"),
	)

	// The server got the document once, then each saved text as a whole,
	// under a version that goes up.
	checkoutURI := fileURI(filepath.Join(project, "checkout.py"))
	definitionRequest := lspCall{Action: "request", Method: "textDocument/definition", Params: map[string]any{
		"textDocument": map[string]any{"uri": checkoutURI},
		"position":     map[string]any{"line": float64(4), "character": float64(11)},
	}}
	didChange := func(version int, text string) lspCall {
		return lspCall{Action: "notify", Method: "textDocument/didChange", Params: map[string]any{
			"textDocument":   map[string]any{"uri": checkoutURI, "version": float64(version)},
			"contentChanges": []any{map[string]any{"text": text}},
		}}
	}
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
		definitionRequest,
		didChange(2, discountedCheckoutSource),
		definitionRequest,
		didChange(3, checkoutSource),
		definitionRequest,
	}
	if got := lsp.received(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("LSP transport received %s\nwant %s", toJSON(t, got), toJSON(t, wantCalls))
	}

	// The file ends with the pricing import, and the server's copy matches it.
	onDisk := readFile(t, filepath.Join(project, "checkout.py"))
	if onDisk != checkoutSource {
		t.Errorf("checkout.py contains %q, want the pricing import restored", onDisk)
	}
	if copy, _ := lsp.copyOf(filepath.Join(project, "checkout.py")); copy.Text != onDisk || copy.Version != 3 {
		t.Errorf("the server's copy of checkout.py is %+v, want version 3 of the file on disk", copy)
	}
}

func TestLSPWriteOpensDocumentThatServerDoesNotHave(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	tools := newConnectedToolbox(t)
	ctx := context.Background()

	// The first thing to happen to checkout.py is a Write: the server gets
	// the document with its new text.
	result, err := tools.Call(ctx, "Write", jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": discountedCheckoutSource}))
	if err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	if result != wroteCheckout(discountedCheckoutSource) {
		t.Errorf("Write = %q, want only the report of the write", result)
	}
	wantOpen := lspCall{Action: "notify", Method: "textDocument/didOpen", Params: map[string]any{
		"textDocument": map[string]any{
			"uri":        fileURI(filepath.Join(project, "checkout.py")),
			"languageId": "python",
			"version":    float64(1),
			"text":       discountedCheckoutSource,
		},
	}}
	if calls := lsp.received(); len(calls) != 2 || !reflect.DeepEqual(calls[1], wantOpen) {
		t.Fatalf("LSP transport received %s\nwant the configuration request, then %s", toJSON(t, calls), toJSON(t, wantOpen))
	}

	// Navigation and Write agree that the document is now open, at version 1.
	if got, err := tools.Call(ctx, "LSP", goToCheckoutTotal); err != nil || got != "Defined in discounts.py:1:5" {
		t.Errorf("LSP = %q, %v; want the definition found in the text that was written", got, err)
	}
	if _, err := tools.Call(ctx, "Write", jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": checkoutSource})); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	want := []string{
		"textDocument/didOpen checkout.py v1",
		"textDocument/didChange checkout.py v2",
	}
	if got := lsp.notifications(); !reflect.DeepEqual(got, want) {
		t.Errorf("the server was notified of %q\nwant %q", got, want)
	}
}

func TestLSPWriteNotifiesServerOfSupportedFilesOnly(t *testing.T) {
	tests := []struct {
		name              string
		filePath          func(project string) string
		wantErr           string
		wantNotifications []string
		wantServerCopy    string // of the file that was written, if the server has it
	}{
		{
			name:              "new file",
			filePath:          func(string) string { return "refunds.py" },
			wantNotifications: []string{"textDocument/didOpen refunds.py v1"},
			wantServerCopy:    "new content\n",
		},
		{
			name:              "file in a new directory",
			filePath:          func(string) string { return "billing/tax.py" },
			wantNotifications: []string{"textDocument/didOpen tax.py v1"},
			wantServerCopy:    "new content\n",
		},
		{
			name:              "existing file that the server was not sent",
			filePath:          func(string) string { return "pricing.py" },
			wantNotifications: []string{"textDocument/didOpen pricing.py v1"},
			wantServerCopy:    "new content\n",
		},
		{
			name:              "file that the server already has, by another spelling of its path",
			filePath:          func(project string) string { return filepath.Join(project, ".", "checkout.py") },
			wantNotifications: []string{"textDocument/didOpen checkout.py v1", "textDocument/didChange checkout.py v2"},
			wantServerCopy:    "new content\n",
		},
		{
			name:     "file of a language that the server does not handle",
			filePath: func(string) string { return "notes.txt" },
		},
		{
			name:     "write that fails",
			filePath: func(project string) string { return project },
			wantErr:  "is a directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := newInvoiceProject(t)
			lsp := newFakeLSP(t)
			followCheckoutImport(lsp, project)
			tools := newConnectedToolbox(t)
			ctx := context.Background()
			if strings.HasSuffix(tt.filePath(project), "checkout.py") {
				// The server has checkout.py from an earlier lookup.
				if _, err := tools.Call(ctx, "LSP", goToCheckoutTotal); err != nil {
					t.Fatalf("LSP returned an error: %v", err)
				}
			}

			result, err := tools.Call(ctx, "Write", jsonArgs(t, map[string]any{"file_path": tt.filePath(project), "content": "new content\n"}))

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Write = %q, %v; want an error mentioning %q", result, err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Write returned an error: %v", err)
			} else if strings.Contains(result, "Warning") {
				t.Errorf("Write = %q, want no warning", result)
			}
			if got := lsp.notifications(); !reflect.DeepEqual(got, tt.wantNotifications) {
				t.Errorf("the server was notified of %q\nwant %q", got, tt.wantNotifications)
			}
			if tt.wantServerCopy != "" {
				path, _ := filepath.Abs(tt.filePath(project))
				if copy, _ := lsp.copyOf(path); copy.Text != tt.wantServerCopy {
					t.Errorf("the server's copy of the file is %q, want %q", copy.Text, tt.wantServerCopy)
				}
			}
		})
	}
}

func TestLSPWriteKeepsVersionWhenChangeNotificationFails(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	tools := newConnectedToolbox(t)
	ctx := context.Background()
	write := func(content string) string {
		t.Helper()
		result, err := tools.Call(ctx, "Write", jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": content}))
		if err != nil {
			t.Fatalf("Write returned an error: %v", err)
		}
		return result
	}
	if _, err := tools.Call(ctx, "LSP", goToCheckoutTotal); err != nil {
		t.Fatalf("LSP returned an error: %v", err)
	}

	// The change does not reach the server: the file is saved all the same,
	// and the model is told that the server is behind.
	lsp.failNext("textDocument/didChange")
	result := write(discountedCheckoutSource)

	wantResult := wroteCheckout(discountedCheckoutSource) + "\n" +
		"Warning: the language server may be out of date: LSP transport: scripted failure of textDocument/didChange"
	if result != wantResult {
		t.Errorf("Write = %q, want the report of the write and a warning about the server", result)
	}
	if got := readFile(t, filepath.Join(project, "checkout.py")); got != discountedCheckoutSource {
		t.Errorf("checkout.py contains %q, want the text that was written", got)
	}

	// The version that failed to go through is the one of the next change.
	if result := write(checkoutSource); strings.Contains(result, "Warning") {
		t.Errorf("Write = %q, want no warning", result)
	}
	write(discountedCheckoutSource)

	want := []string{
		"textDocument/didOpen checkout.py v1",
		"textDocument/didChange checkout.py v2", // failed
		"textDocument/didChange checkout.py v2",
		"textDocument/didChange checkout.py v3",
	}
	if got := lsp.notifications(); !reflect.DeepEqual(got, want) {
		t.Errorf("the server was notified of %q\nwant %q", got, want)
	}
	if copy, _ := lsp.copyOf(filepath.Join(project, "checkout.py")); copy.Text != discountedCheckoutSource || copy.Version != 3 {
		t.Errorf("the server's copy of checkout.py is %+v, want version 3 of the file on disk", copy)
	}
}

func TestLSPWriteOpensDocumentAgainWhenOpeningFails(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	tools := newConnectedToolbox(t)
	ctx := context.Background()
	arguments := jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": discountedCheckoutSource})

	lsp.failNext("textDocument/didOpen")
	result, err := tools.Call(ctx, "Write", arguments)

	if err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	if !strings.Contains(result, "Warning: the language server may be out of date: ") {
		t.Errorf("Write = %q, want a warning about the server", result)
	}
	// The document is not open: the next Write opens it, rather than change it.
	if _, err := tools.Call(ctx, "Write", arguments); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	want := []string{
		"textDocument/didOpen checkout.py v1", // failed
		"textDocument/didOpen checkout.py v1",
	}
	if got := lsp.notifications(); !reflect.DeepEqual(got, want) {
		t.Errorf("the server was notified of %q\nwant %q", got, want)
	}
	if copy, open := lsp.copyOf(filepath.Join(project, "checkout.py")); !open || copy.Text != discountedCheckoutSource {
		t.Errorf("the server's copy of checkout.py is %+v, want the text that was written", copy)
	}
}

func TestLSPBashTellsServerAboutFilesChangedByCommands(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	lsp.script("textDocument/references", []any{})
	tools := newConnectedToolbox(t)
	ctx := context.Background()
	bash := func(command string) string {
		t.Helper()
		result, err := tools.Call(ctx, "Bash", jsonArgs(t, map[string]any{"command": command}))
		if err != nil {
			t.Fatalf("Bash(%s) returned an error: %v", command, err)
		}
		return result
	}
	// The server has checkout.py and reports.py, not pricing.py.
	for _, arguments := range []string{
		goToCheckoutTotal,
		`{"operation": "findReferences", "filePath": "reports.py", "line": 1, "character": 21}`,
	} {
		if _, err := tools.Call(ctx, "LSP", arguments); err != nil {
			t.Fatalf("LSP(%s) returned an error: %v", arguments, err)
		}
	}

	// A command edits a file that the server has: the server gets the change.
	result := bash(`sed -i.bak 's/from pricing/from discounts/' checkout.py && rm checkout.py.bak`)

	if result != "(no output)" {
		t.Errorf("Bash = %q, want only the output of the command", result)
	}
	if got, err := tools.Call(ctx, "LSP", goToCheckoutTotal); err != nil || got != "Defined in discounts.py:1:5" {
		t.Errorf("LSP = %q, %v; want the definition found in the text that the command saved", got, err)
	}

	// Commands that leave the server's files alone are not worth a word: one
	// that changes nothing, one that fails, one that edits a file the server
	// does not have, one that deletes a file the server has.
	bash(`cat checkout.py > /dev/null`)
	bash(`exit 3`)
	bash(`echo '# reviewed' >> pricing.py`)
	bash(`rm reports.py`)

	want := []string{
		"textDocument/didOpen checkout.py v1",
		"textDocument/didOpen reports.py v1",
		"textDocument/didChange checkout.py v2",
	}
	if got := lsp.notifications(); !reflect.DeepEqual(got, want) {
		t.Errorf("the server was notified of %q\nwant %q", got, want)
	}
	if copy, _ := lsp.copyOf(filepath.Join(project, "checkout.py")); copy.Text != discountedCheckoutSource {
		t.Errorf("the server's copy of checkout.py is %q, want the file on disk %q", copy.Text, discountedCheckoutSource)
	}
}

func TestLSPBashReportsServerLeftBehind(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	tools := newConnectedToolbox(t)
	ctx := context.Background()
	if _, err := tools.Call(ctx, "LSP", goToCheckoutTotal); err != nil {
		t.Fatalf("LSP returned an error: %v", err)
	}
	lsp.failNext("textDocument/didChange")

	result, err := tools.Call(ctx, "Bash", `{"command": "echo '# reviewed' >> checkout.py && echo done"}`)

	if err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if !strings.HasPrefix(result, "done\nWarning: the language server may be out of date: ") {
		t.Errorf("Bash = %q, want the output of the command and a warning about the server", result)
	}
	// The change that the server missed is sent again after the next command.
	if _, err := tools.Call(ctx, "Bash", `{"command": "true"}`); err != nil {
		t.Fatalf("Bash returned an error: %v", err)
	}
	if copy, _ := lsp.copyOf(filepath.Join(project, "checkout.py")); copy.Text != checkoutSource+"# reviewed\n" || copy.Version != 2 {
		t.Errorf("the server's copy of checkout.py is %+v, want version 2 of the file on disk", copy)
	}
}
