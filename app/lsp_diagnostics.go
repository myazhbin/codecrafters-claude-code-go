package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// diagnostic is an issue that the server found in a document, such as a type
// error.
type diagnostic struct {
	Range lspRange `json:"range"`
	// Severity is 1 for an error, 2 for a warning, 3 for an information and
	// 4 for a hint.
	Severity int    `json:"severity"`
	Message  string `json:"message"`
}

// diagnosticReport is the content of a textDocument/publishDiagnostics
// notification: all the issues of a document.
type diagnosticReport struct {
	URI string `json:"uri"`
	// Version is the version of the document that the report is about, when
	// the server tells.
	Version     *int         `json:"version"`
	Diagnostics []diagnostic `json:"diagnostics"`
}

// newDiagnostics returns the issues that the server has reported since it was
// last asked and that the model has not been told about yet, in the form that
// is added to a tool result, or "" if there is none:
//
//	<new-diagnostics>The following new diagnostic issues were detected:
//
//	checkout.py:
//	  [Line 5:12] Type Price is not assignable to return type int</new-diagnostics>
//
// The server analyzes the project in the background, so an edit to a file can
// bring up issues some time later, and in other files.
func (s *languageServer) newDiagnostics() string {
	// The latest report of a file replaces the previous ones.
	var files []string // in the order of their first report
	latest := map[string][]diagnostic{}
	for _, report := range s.readReports() {
		if _, seen := latest[report.URI]; !seen {
			files = append(files, report.URI)
		}
		latest[report.URI] = report.Diagnostics
	}

	var sections []string
	for _, uri := range files {
		var issues []string
		for _, issue := range latest[uri] {
			if !slices.Contains(s.reported[uri], issue) {
				issues = append(issues, "  "+issue.String())
			}
		}
		s.reported[uri] = latest[uri]
		if len(issues) > 0 {
			sections = append(sections, displayPath(pathFromURI(uri))+":\n"+strings.Join(issues, "\n"))
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return "<new-diagnostics>The following new diagnostic issues were detected:\n\n" +
		strings.Join(sections, "\n\n") + "</new-diagnostics>"
}

// readReports reads the notifications that the server has queued since the
// last read, without waiting for more, and returns the diagnostic reports
// among them that are current: a report made for another version of a file
// than the one the server was last sent is about code that is gone.
func (s *languageServer) readReports() []diagnosticReport {
	response, err := s.client.Call("notifications", map[string]any{"after": s.cursor, "timeoutMs": 0})
	if err != nil {
		fmt.Fprintf(s.log, "warning: cannot read the notifications of the language server: %v\n", err)
		return nil
	}
	var batch struct {
		Cursor        json.RawMessage `json:"cursor"`
		Notifications []struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(response, &batch); err != nil {
		fmt.Fprintf(s.log, "warning: cannot read the notifications of the language server: %v\n", err)
		return nil
	}
	if len(batch.Cursor) > 0 {
		s.cursor = batch.Cursor
	}

	var reports []diagnosticReport
	for _, notification := range batch.Notifications {
		if notification.Method != "textDocument/publishDiagnostics" {
			continue
		}
		var report diagnosticReport
		if err := json.Unmarshal(notification.Params, &report); err != nil {
			fmt.Fprintf(s.log, "warning: cannot read a diagnostic report of the language server: %v\n", err)
			continue
		}
		// The server may not write the URI of a file the way the agent does.
		if path := pathFromURI(report.URI); path != report.URI {
			report.URI = fileURI(path)
		}
		if report.Version != nil {
			document := s.documents[report.URI]
			if document == nil || document.version != *report.Version {
				continue
			}
		}
		reports = append(reports, report)
	}
	return reports
}

// String renders the issue the way the model is told about it, as in
// "[Line 5:12] Type Price is not assignable to return type int": where it
// starts, counted from one, and what it is, with its severity when it is less
// than an error.
func (d diagnostic) String() string {
	description := fmt.Sprintf("[Line %d:%d] %s", d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
	switch d.Severity {
	case 2:
		description += " (warning)"
	case 3:
		description += " (information)"
	case 4:
		description += " (hint)"
	}
	return description
}
