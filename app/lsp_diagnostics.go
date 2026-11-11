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

// fileIssue is an issue in the file that a URI identifies.
type fileIssue struct {
	uri   string
	issue diagnostic
}

// newDiagnostics returns the issues that the server has reported and that the
// model has not been told about yet, in the form that is added to a tool
// result, or "" if there is none:
//
//	<new-diagnostics>The following new diagnostic issues were detected:
//
//	checkout.py:
//	  [Line 5:12] Type Price is not assignable to return type int</new-diagnostics>
//
// The server analyzes the project in the background, so an edit to a file can
// bring up issues some time later, and in other files.
func (s *languageServer) newDiagnostics() string {
	s.collectDiagnostics()
	if len(s.pending) == 0 {
		return ""
	}

	var files []string // in the order their issues came up
	issues := map[string][]string{}
	for _, news := range s.pending {
		file := displayPath(pathFromURI(news.uri))
		if _, seen := issues[file]; !seen {
			files = append(files, file)
		}
		issues[file] = append(issues[file], "  "+news.issue.String())
	}
	s.pending = nil

	sections := make([]string, len(files))
	for i, file := range files {
		sections[i] = file + ":\n" + strings.Join(issues[file], "\n")
	}
	return "<new-diagnostics>The following new diagnostic issues were detected:\n\n" +
		strings.Join(sections, "\n\n") + "</new-diagnostics>"
}

// collectDiagnostics takes in the reports that the server has made since the
// last time, and sets aside the issues that are news to the model: those that
// the previous report of their file did not have. A report replaces the
// previous one of its file as what the model is taken to know, so that an
// issue that goes away and comes back is news again.
//
// A report is weighed against the versions of the files at the time it is
// read. So the reports are collected before each change that is sent to the
// server, and not just before each request to the model: after the change, a
// report that was right about a file when the server made it would look out
// of date.
func (s *languageServer) collectDiagnostics() {
	for _, report := range s.readReports() {
		for _, issue := range report.Diagnostics {
			news := fileIssue{uri: report.URI, issue: issue}
			if !slices.Contains(s.reported[report.URI], issue) && !slices.Contains(s.pending, news) {
				s.pending = append(s.pending, news)
			}
		}
		s.reported[report.URI] = report.Diagnostics
	}
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
