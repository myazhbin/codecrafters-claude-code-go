package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// migratedPricingSource is pricing.py once total returns a Price rather than
// an integer, which breaks its callers.
const migratedPricingSource = "from typing import TypedDict\n" +
	"\n" +
	"\n" +
	"class Price(TypedDict):\n" +
	"    cents: int\n" +
	"\n" +
	"\n" +
	"def total(items) -> Price:\n" +
	"    return {\"cents\": sum(item.price * item.quantity for item in items)}\n"

// brokenCaller is the issue that the migration of pricing.total brings up in
// checkout.py, as the server reports it and as the model is told about it.
var (
	brokenCaller       = issueAt(4, 11, 4, 32, 1, "Type Price is not assignable to return type int")
	brokenCallerReport = "<new-diagnostics>The following new diagnostic issues were detected:\n" +
		"\n" +
		"checkout.py:\n" +
		"  [Line 5:12] Type Price is not assignable to return type int</new-diagnostics>"
)

func TestRunAddsNewDiagnosticsToLatestToolResult(t *testing.T) {
	const prompt = "Change pricing.total to return a Price TypedDict. Fix the type errors your tools report."
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	// The edit of pricing.py brings up an issue in checkout.py, which the
	// server has at version 1 and which nobody edited.
	lsp.onDocument = func(path string, _ fakeDocument) {
		if filepath.Base(path) == "pricing.py" {
			lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 1, brokenCaller)
		}
	}
	lookup := toolCall{id: "call_1", name: "LSP", arguments: goToCheckoutTotal}
	write := toolCall{id: "call_2", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "pricing.py", "content": migratedPricingSource})}
	read := toolCall{id: "call_3", name: "Read", arguments: `{"file_path": "checkout.py"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(lookup),
		assistantToolCalls(write),
		assistantToolCalls(read),
		assistantText("checkout.py needs a fix."),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "checkout.py needs a fix." {
		t.Errorf("stdout = %q, want the model's answer", stdout)
	}
	requests := llm.recorded()
	if len(requests) != 4 {
		t.Fatalf("LLM received %d requests, want 4", len(requests))
	}
	wrote := fmt.Sprintf("Wrote %d bytes to pricing.py", len(migratedPricingSource))
	// The report was there before the request that followed the Write: it
	// goes out with the latest tool result, after the result of the Write.
	assertMessages(t, requests[2],
		userMessage(prompt),
		assistantMessage("", lookup),
		toolMessage("call_1", "Defined in pricing.py:1:5"),
		assistantMessage("", write),
		toolMessage("call_2", wrote+"\n\n"+brokenCallerReport),
	)
	// It is not repeated with the tool results that follow.
	assertMessages(t, requests[3],
		userMessage(prompt),
		assistantMessage("", lookup),
		toolMessage("call_1", "Defined in pricing.py:1:5"),
		assistantMessage("", write),
		toolMessage("call_2", wrote+"\n\n"+brokenCallerReport),
		assistantMessage("", read),
		toolMessage("call_3", checkoutSource),
	)
}

func TestRunAddsDiagnosticsThatArriveLaterToLaterToolResult(t *testing.T) {
	const prompt = "Change pricing.total to return a Price TypedDict. Fix the type errors your tools report."
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	write := toolCall{id: "call_1", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "pricing.py", "content": migratedPricingSource})}
	read := toolCall{id: "call_2", name: "Read", arguments: `{"file_path": "checkout.py"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(write),
		assistantToolCalls(read),
		assistantText("checkout.py needs a fix."),
	)
	// The server takes its time: its report only arrives while the model is
	// answering the request that followed the Write.
	llm.onRequest = func(number int) {
		if number == 2 {
			lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 0, brokenCaller)
		}
	}

	_, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	wrote := fmt.Sprintf("Wrote %d bytes to pricing.py", len(migratedPricingSource))
	assertMessages(t, requests[1],
		userMessage(prompt),
		assistantMessage("", write),
		toolMessage("call_1", wrote),
	)
	// The result of the Write stays as it was: the report comes with the
	// result of the tool that ran next.
	assertMessages(t, requests[2],
		userMessage(prompt),
		assistantMessage("", write),
		toolMessage("call_1", wrote),
		assistantMessage("", read),
		toolMessage("call_2", checkoutSource+"\n\n"+brokenCallerReport),
	)
}

func TestRunAddsDiagnosticsToTheLastToolResultOfATurn(t *testing.T) {
	project := newInvoiceProject(t)
	writeFile(t, filepath.Join(project, "empty.txt"), "")
	lsp := newFakeLSP(t)
	lsp.onDocument = func(string, fakeDocument) {
		lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 0, brokenCaller)
	}
	write := toolCall{id: "call_1", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "pricing.py", "content": migratedPricingSource})}
	readEmpty := toolCall{id: "call_2", name: "Read", arguments: `{"file_path": "empty.txt"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(write, readEmpty),
		assistantText("done"),
	)

	_, stderr, code := runCLI(t, llm, "-p", "Migrate pricing.total.")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	// A report alone makes the result of a tool that had nothing to say.
	assertMessages(t, llm.recorded()[1],
		userMessage("Migrate pricing.total."),
		assistantMessage("", write, readEmpty),
		toolMessage("call_1", fmt.Sprintf("Wrote %d bytes to pricing.py", len(migratedPricingSource))),
		toolMessage("call_2", brokenCallerReport),
	)
}

func TestRunTellsModelAboutIssueThatLaterWriteOfTheSameTurnFixes(t *testing.T) {
	const prompt = "Change pricing.total to return a Price TypedDict. Fix the type errors your tools report."
	const fixedCheckoutSource = "from pricing import total as invoice_total\n" +
		"\n" +
		"\n" +
		"def checkout(items):\n" +
		"    return invoice_total(items)[\"cents\"]\n"
	project := newInvoiceProject(t)
	checkout := filepath.Join(project, "checkout.py")
	lsp := newFakeLSP(t)
	followCheckoutImport(lsp, project)
	// The server reports on checkout.py as soon as a change calls for it: the
	// new pricing.py breaks it, and its own new text repairs it.
	lsp.onDocument = func(path string, document fakeDocument) {
		switch filepath.Base(path) {
		case "pricing.py":
			if copy, open := lsp.copyOf(checkout); open {
				lsp.publishDiagnostics(checkout, copy.Version, brokenCaller)
			}
		case "checkout.py":
			lsp.publishDiagnostics(checkout, document.Version)
		}
	}
	lookup := toolCall{id: "call_1", name: "LSP", arguments: goToCheckoutTotal}
	writePricing := toolCall{id: "call_2", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "pricing.py", "content": migratedPricingSource})}
	writeCheckout := toolCall{id: "call_3", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": fixedCheckoutSource})}
	writeReports := toolCall{id: "call_4", name: "Write", arguments: jsonArgs(t, map[string]any{"file_path": "reports.py", "content": reportsSource})}
	llm := newFakeLLM(t,
		assistantToolCalls(lookup),
		// All the edits come in one turn: the model is not asked anything
		// between the Write that breaks checkout.py and the one that fixes it.
		assistantToolCalls(writePricing, writeCheckout, writeReports),
		assistantText("Migrated."),
	)

	_, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	requests := llm.recorded()
	if len(requests) != 3 {
		t.Fatalf("LLM received %d requests, want 3", len(requests))
	}
	// The report on the broken checkout.py was right when the server made it.
	// It reaches the model, although checkout.py has changed by the time the
	// model is next asked.
	assertMessages(t, requests[2],
		userMessage(prompt),
		assistantMessage("", lookup),
		toolMessage("call_1", "Defined in pricing.py:1:5"),
		assistantMessage("", writePricing, writeCheckout, writeReports),
		toolMessage("call_2", fmt.Sprintf("Wrote %d bytes to pricing.py", len(migratedPricingSource))),
		toolMessage("call_3", fmt.Sprintf("Wrote %d bytes to checkout.py", len(fixedCheckoutSource))),
		toolMessage("call_4", fmt.Sprintf("Wrote %d bytes to reports.py", len(reportsSource))+"\n\n"+brokenCallerReport),
	)
}

func TestNewDiagnosticsWeighsReportsBeforeTheirDocumentChanges(t *testing.T) {
	project, lsp, server, _ := newDiagnosticsFixture(t)
	checkout := filepath.Join(project, "checkout.py")
	tools := baseToolbox(server)
	// A report about version 2 of checkout.py is waiting when version 3 is
	// written. A report about version 2 that comes after that is out of date.
	lsp.publishDiagnostics(checkout, 2, brokenCaller)
	if _, err := tools.Call(context.Background(), "Write", jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": checkoutSource})); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	lsp.publishDiagnostics(checkout, 2, issueAt(0, 0, 0, 4, 1, "Stale issue of the previous text"))

	got := server.newDiagnostics()

	if got != brokenCallerReport {
		t.Errorf("newDiagnostics = %q\nwant %q", got, brokenCallerReport)
	}
}

func TestRunDoesNotReadDiagnosticsBeforeThereIsAToolResult(t *testing.T) {
	project := newInvoiceProject(t)
	lsp := newFakeLSP(t)
	lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 0, brokenCaller)
	llm := newFakeLLM(t, assistantText("hello"))

	_, stderr, code := runCLI(t, llm, "-p", "Say hello.")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	assertMessages(t, llm.onlyRequest(), userMessage("Say hello."))
	if cursors := lsp.cursors(); len(cursors) != 0 {
		t.Errorf("the queue of notifications was read %d times, want it left alone until a tool result can carry its reports", len(cursors))
	}
}

// newDiagnosticsFixture sets up the invoice project and a language server
// that has checkout.py at version 2, after a lookup and a Write, and nothing
// else.
func newDiagnosticsFixture(t *testing.T) (project string, lsp *fakeLSP, server *languageServer, log *bytes.Buffer) {
	t.Helper()
	project = newInvoiceProject(t)
	lsp = newFakeLSP(t)
	followCheckoutImport(lsp, project)
	log = &bytes.Buffer{}
	server = connectLSP(log)
	if server == nil {
		t.Fatal("cannot connect to the fake LSP transport")
	}
	tools := baseToolbox(server)
	if _, err := tools.Call(context.Background(), "LSP", goToCheckoutTotal); err != nil {
		t.Fatalf("LSP returned an error: %v", err)
	}
	if _, err := tools.Call(context.Background(), "Write", jsonArgs(t, map[string]any{"file_path": "checkout.py", "content": discountedCheckoutSource})); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	return project, lsp, server, log
}

// newDiagnosticsBlock is the feedback that tells the model about new issues,
// given the sections of the files they are in.
func newDiagnosticsBlock(sections ...string) string {
	return "<new-diagnostics>The following new diagnostic issues were detected:\n\n" +
		strings.Join(sections, "\n\n") + "</new-diagnostics>"
}

func TestNewDiagnosticsKeepsCurrentReportsOnly(t *testing.T) {
	stale := issueAt(0, 0, 0, 4, 1, "Stale issue of the previous text")
	current := issueAt(4, 11, 4, 32, 1, "Type Price is not assignable to return type int")
	const currentSection = "checkout.py:\n  [Line 5:12] Type Price is not assignable to return type int"
	tests := []struct {
		name    string
		publish func(lsp *fakeLSP, project string)
		want    string
	}{
		{
			name: "report for the version last sent",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 2, current)
			},
			want: newDiagnosticsBlock(currentSection),
		},
		{
			name: "report for an older version",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 1, stale)
			},
			want: "",
		},
		{
			name: "report for a version that was not sent yet",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 3, stale)
			},
			want: "",
		},
		{
			name: "report without a version",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 0, current)
			},
			want: newDiagnosticsBlock(currentSection),
		},
		{
			name: "report without a version for a file that the server was not sent",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "reports.py"), 0, issueAt(4, 11, 4, 16, 1, "Operator - not supported for Price"))
			},
			want: newDiagnosticsBlock("reports.py:\n  [Line 5:12] Operator - not supported for Price"),
		},
		{
			name: "report with a version for a file that the server was not sent",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "reports.py"), 1, stale)
			},
			want: "",
		},
		{
			name: "stale report before the current one",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 1, stale)
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 2, current)
			},
			want: newDiagnosticsBlock(currentSection),
		},
		{
			name: "stale report after the current one does not replace it",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 2, current)
				lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 1, stale)
			},
			want: newDiagnosticsBlock(currentSection),
		},
		{
			name: "notifications that are not diagnostic reports",
			publish: func(lsp *fakeLSP, project string) {
				lsp.publish("window/logMessage", map[string]any{"type": 3, "message": "Type Price is not assignable"})
				lsp.publish("$/progress", map[string]any{"token": "analysis", "value": map[string]any{"kind": "end"}})
			},
			want: "",
		},
		{
			name:    "nothing new",
			publish: func(*fakeLSP, string) {},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, lsp, server, log := newDiagnosticsFixture(t)
			tt.publish(lsp, project)

			got := server.newDiagnostics()

			if got != tt.want {
				t.Errorf("newDiagnostics = %q\nwant %q", got, tt.want)
			}
			if log.Len() > len("LSP client connected\n") {
				t.Errorf("log = %q, want nothing but the startup line", log)
			}
		})
	}
}

func TestNewDiagnosticsTellsOnlyAboutIssuesThatAreNew(t *testing.T) {
	project, lsp, server, _ := newDiagnosticsFixture(t)
	checkout := filepath.Join(project, "checkout.py")
	returnType := issueAt(4, 11, 4, 32, 1, "Type Price is not assignable to return type int")
	unusedImport := issueAt(0, 22, 0, 27, 2, "Import total is not accessed")
	const returnTypeLine = "  [Line 5:12] Type Price is not assignable to return type int"
	const unusedImportLine = "  [Line 1:23] Import total is not accessed (warning)"
	steps := []struct {
		name    string
		publish func()
		want    string
	}{
		{
			name:    "first report",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType) },
			want:    newDiagnosticsBlock("checkout.py:\n" + returnTypeLine),
		},
		{
			name:    "no report since",
			publish: func() {},
			want:    "",
		},
		{
			name:    "report with one more issue: only that one is new",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType, unusedImport) },
			want:    newDiagnosticsBlock("checkout.py:\n" + unusedImportLine),
		},
		{
			name:    "same report again",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType, unusedImport) },
			want:    "",
		},
		{
			name:    "report with one issue less",
			publish: func() { lsp.publishDiagnostics(checkout, 2, unusedImport) },
			want:    "",
		},
		{
			name:    "issue that comes back after it was gone",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType, unusedImport) },
			want:    newDiagnosticsBlock("checkout.py:\n" + returnTypeLine),
		},
		{
			name:    "empty report clears the issues of the file",
			publish: func() { lsp.publishDiagnostics(checkout, 2) },
			want:    "",
		},
		{
			name:    "issues that come back after they were cleared",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType, unusedImport) },
			want:    newDiagnosticsBlock("checkout.py:\n" + returnTypeLine + "\n" + unusedImportLine),
		},
		{
			name: "several reports at once: the news of each is told, once",
			publish: func() {
				lsp.publishDiagnostics(checkout, 2)
				lsp.publishDiagnostics(checkout, 2, returnType)
				lsp.publishDiagnostics(checkout, 2)
				lsp.publishDiagnostics(checkout, 2, returnType, unusedImport)
				lsp.publishDiagnostics(checkout, 2, unusedImport)
			},
			want: newDiagnosticsBlock("checkout.py:\n" + returnTypeLine + "\n" + unusedImportLine),
		},
		{
			name:    "the latest of them is what the model is taken to know",
			publish: func() { lsp.publishDiagnostics(checkout, 2, returnType, unusedImport) },
			want:    newDiagnosticsBlock("checkout.py:\n" + returnTypeLine),
		},
	}
	for _, step := range steps {
		step.publish()

		got := server.newDiagnostics()

		if got != step.want {
			t.Fatalf("%s: newDiagnostics = %q\nwant %q", step.name, got, step.want)
		}
	}
	// Every read of the queue starts where the previous one ended, including
	// after a read that found nothing. The first two reads are those that
	// came before the document was opened and before it was changed.
	if got, want := lsp.cursors(), []int{0, 0, 0, 1, 1, 2, 3, 4, 5, 6, 7, 12}; !reflect.DeepEqual(got, want) {
		t.Errorf("the queue of notifications was read after %v, want %v", got, want)
	}
}

func TestNewDiagnosticsDescribesIssuesByFile(t *testing.T) {
	project, lsp, server, _ := newDiagnosticsFixture(t)
	outside := writeTempFile(t, "library.py", "import nothing\n")
	lsp.publishDiagnostics(filepath.Join(project, "reports.py"), 0,
		issueAt(4, 11, 4, 16, 1, "Operator \"-\" not supported for types \"Price\" and \"Price\"\n  Operator \"-\" not supported for types \"Price\" and \"Price\""),
		issueAt(0, 0, 0, 4, 0, "Issue without a severity"),
	)
	lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 2,
		issueAt(4, 11, 4, 32, 1, "Type Price is not assignable to return type int"),
		issueAt(0, 22, 0, 27, 2, "Import total is not accessed"),
		issueAt(9, 0, 9, 1, 3, "Consider a docstring"),
		issueAt(99, 7, 99, 9, 4, "Unreachable code"),
	)
	lsp.publishDiagnostics(filepath.Join(project, "billing", "tax.py"), 0, issueAt(2, 4, 2, 9, 1, "Undefined name rate"))
	lsp.publishDiagnostics(outside, 0, issueAt(0, 7, 0, 14, 1, "Import nothing could not be resolved"))
	// A second report for a file keeps the file where it first came up.
	lsp.publishDiagnostics(filepath.Join(project, "reports.py"), 0,
		issueAt(4, 11, 4, 16, 1, "Operator \"-\" not supported for types \"Price\" and \"Price\"\n  Operator \"-\" not supported for types \"Price\" and \"Price\""),
		issueAt(0, 0, 0, 4, 0, "Issue without a severity"),
	)

	got := server.newDiagnostics()

	want := newDiagnosticsBlock(
		"reports.py:\n"+
			"  [Line 5:12] Operator \"-\" not supported for types \"Price\" and \"Price\"\n"+
			"  Operator \"-\" not supported for types \"Price\" and \"Price\"\n"+
			"  [Line 1:1] Issue without a severity",
		"checkout.py:\n"+
			"  [Line 5:12] Type Price is not assignable to return type int\n"+
			"  [Line 1:23] Import total is not accessed (warning)\n"+
			"  [Line 10:1] Consider a docstring (information)\n"+
			"  [Line 100:8] Unreachable code (hint)",
		filepath.Join("billing", "tax.py")+":\n"+
			"  [Line 3:5] Undefined name rate",
		outside+":\n"+
			"  [Line 1:8] Import nothing could not be resolved",
	)
	if got != want {
		t.Errorf("newDiagnostics = %q\nwant %q", got, want)
	}
}

func TestNewDiagnosticsReadsQueueAgainAfterFailedRead(t *testing.T) {
	project, lsp, server, log := newDiagnosticsFixture(t)
	lsp.publishDiagnostics(filepath.Join(project, "checkout.py"), 2, brokenCaller)
	lsp.failNext("notifications")

	got := server.newDiagnostics()

	if got != "" {
		t.Errorf("newDiagnostics = %q, want nothing when the queue cannot be read", got)
	}
	if !strings.Contains(log.String(), "warning: cannot read the notifications of the language server") {
		t.Errorf("log = %q, want a warning about the failed read", log)
	}

	// The reports that the failed read missed are not lost.
	got = server.newDiagnostics()

	if got != brokenCallerReport {
		t.Errorf("newDiagnostics = %q\nwant %q", got, brokenCallerReport)
	}
	// The read that failed gave no cursor: the next one starts from the same.
	cursors := lsp.cursors()
	if got, want := cursors[len(cursors)-2:], []int{0, 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the queue of notifications was last read after %v, want %v", got, want)
	}
}
