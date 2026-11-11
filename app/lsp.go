package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
)

// languageServer is the agent's side of the conversation with a language
// server: on top of the transport, it keeps track of what the server has been
// told about the project's files.
type languageServer struct {
	client *LSPClient
	// log receives what goes wrong behind the scenes.
	log io.Writer
	// documents holds the files sent to the server, by URI. The server needs
	// a file before it can answer about it, rejects a file that is opened
	// twice, and only learns that a file has changed when it is told so.
	documents map[string]*document
	// cursor marks how far the queue of the server's notifications was read.
	cursor json.RawMessage
	// reported holds the issues of the latest report that the model was told
	// about for each file, by URI, so that it is only told about new ones.
	reported map[string][]diagnostic
}

// document is a file as the language server knows it.
type document struct {
	path string
	// version numbers the texts of the file sent to the server: 1 for the
	// text that opened the document, and one more with every change.
	version int
	// text is the latest text of the file sent to the server.
	text string
}

// connectLSP connects to the language server that the environment configures,
// or returns nil if there is none to use: the agent then makes do with its
// other tools.
func connectLSP(log io.Writer) *languageServer {
	client, err := LSPClientFromEnvironment()
	if err != nil {
		fmt.Fprintf(log, "warning: LSP is unavailable: %v\n", err)
		return nil
	}
	if client == nil {
		return nil
	}
	fmt.Fprintln(log, "LSP client connected")
	return &languageServer{
		client:    client,
		log:       log,
		documents: map[string]*document{},
		cursor:    json.RawMessage("0"),
		reported:  map[string][]diagnostic{},
	}
}

// The types below are the parts of the Language Server Protocol that the
// agent speaks. Positions count lines and UTF-16 code units from zero.

// lspMessage is a request or a notification for the language server.
type lspMessage struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspLocation is a range of a document, such as the name of a function in its
// definition.
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

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

// open sends the file at path to the server, unless the server has it
// already, and returns the URI that identifies it.
func (s *languageServer) open(path string) (uri string, err error) {
	uri = fileURI(path)
	if s.documents[uri] != nil {
		return uri, nil
	}
	if !s.handles(path) {
		return "", fmt.Errorf("no language server is configured for files like %s", filepath.Base(path))
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return uri, s.didOpen(path, string(text))
}

// saved tells the server that the file at path was saved with a new text, so
// that its answers are about the code as it now is. Files that the server
// does not analyze are none of its business.
func (s *languageServer) saved(path string) error {
	if !s.handles(path) {
		return nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if document := s.documents[fileURI(path)]; document != nil {
		return s.didChange(document, string(text))
	}
	return s.didOpen(path, string(text))
}

// refresh tells the server about the files it has that were changed behind
// its back, as a shell command can do.
func (s *languageServer) refresh() error {
	var failures []error
	for _, uri := range slices.Sorted(maps.Keys(s.documents)) {
		document := s.documents[uri]
		text, err := os.ReadFile(document.path)
		if err != nil || string(text) == document.text {
			continue // gone or untouched: nothing to tell
		}
		if err := s.didChange(document, string(text)); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// handles reports whether the server analyzes files like the one at path.
func (s *languageServer) handles(path string) bool {
	_, known := s.client.ExtensionToLanguage[filepath.Ext(path)]
	return known
}

// didOpen sends the server a file that it does not have yet.
func (s *languageServer) didOpen(path, text string) error {
	uri := fileURI(path)
	_, err := s.client.Call("notify", lspMessage{
		Method: "textDocument/didOpen",
		Params: map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": s.client.ExtensionToLanguage[filepath.Ext(path)],
				"version":    1,
				"text":       text,
			},
		},
	})
	if err != nil {
		return err
	}
	s.documents[uri] = &document{path: path, version: 1, text: text}
	return nil
}

// didChange sends the server the new text of a file that it has. The text
// replaces the previous one as a whole, under the next version number, which
// only becomes the document's once the server has taken the change.
func (s *languageServer) didChange(document *document, text string) error {
	version := document.version + 1
	_, err := s.client.Call("notify", lspMessage{
		Method: "textDocument/didChange",
		Params: map[string]any{
			"textDocument":   map[string]any{"uri": fileURI(document.path), "version": version},
			"contentChanges": []any{map[string]any{"text": text}},
		},
	})
	if err != nil {
		return err
	}
	document.version, document.text = version, text
	return nil
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

// trackWrites returns the Write tool extended to tell the server about every
// file it saves, before it reports to the model.
func (s *languageServer) trackWrites(write Tool) Tool {
	return thenSync(write, func(arguments string) error {
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := decodeArguments(arguments, &args); err != nil {
			return err
		}
		path, err := filepath.Abs(args.FilePath)
		if err != nil {
			return err
		}
		return s.saved(path)
	})
}

// trackCommands returns the Bash tool extended to tell the server about the
// files that a command has changed, before it reports to the model.
func (s *languageServer) trackCommands(bash Tool) Tool {
	return thenSync(bash, func(string) error {
		return s.refresh()
	})
}

// thenSync returns the tool extended to call sync, with the arguments of the
// call, once it has done its job. A failure of sync does not undo that job: it
// is reported along with the tool's result.
func thenSync(tool Tool, sync func(arguments string) error) Tool {
	run := tool.Run
	tool.Run = func(ctx context.Context, arguments string) (string, error) {
		result, err := run(ctx, arguments)
		if err != nil {
			return result, err
		}
		if err := sync(arguments); err != nil {
			result = appendLine(result, "Warning: the language server may be out of date: "+strings.TrimSpace(err.Error()))
		}
		return result, nil
	}
	return tool
}

// definition returns where the symbol at the given position of the file at
// path is defined.
func (s *languageServer) definition(path string, position lspPosition) ([]lspLocation, error) {
	return s.locate("textDocument/definition", path, position, nil)
}

// references returns every place that refers to the symbol at the given
// position of the file at path, its declaration included.
func (s *languageServer) references(path string, position lspPosition) ([]lspLocation, error) {
	return s.locate("textDocument/references", path, position, map[string]any{
		"context": map[string]any{"includeDeclaration": true},
	})
}

// locate sends the server a request about the symbol at the given position of
// the file at path, after making sure that the server has the file, and
// returns the locations that answer it. options holds the parameters that the
// request takes on top of the document and the position, if any.
func (s *languageServer) locate(method, path string, position lspPosition, options map[string]any) ([]lspLocation, error) {
	uri, err := s.open(path)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     position,
	}
	maps.Copy(params, options)
	result, err := s.client.Call("request", lspMessage{Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	return decodeLocations(result)
}

// decodeLocations reads the result of a request that locates a symbol: one
// location, a list of locations, or null when there is none.
func decodeLocations(result json.RawMessage) ([]lspLocation, error) {
	result = bytes.TrimSpace(result)
	switch {
	case len(result) == 0 || bytes.Equal(result, []byte("null")):
		return nil, nil
	case result[0] == '[':
		var locations []lspLocation
		if err := json.Unmarshal(result, &locations); err != nil {
			return nil, fmt.Errorf("unexpected answer from the language server: %w", err)
		}
		return locations, nil
	default:
		var location lspLocation
		if err := json.Unmarshal(result, &location); err != nil {
			return nil, fmt.Errorf("unexpected answer from the language server: %w", err)
		}
		return []lspLocation{location}, nil
	}
}

// newLSPTool returns the tool that lets the model ask the language server
// about the code of the project.
func newLSPTool(server *languageServer) Tool {
	return Tool{
		Name: "LSP",
		Description: "Asks the language server about the symbol at a position of a file. " +
			"goToDefinition returns the file, line, and character where the symbol is defined. " +
			"findReferences returns the file, line, and character of every reference to the symbol, " +
			"such as the calls of a function, including through aliases. " +
			"Save your edits with the Write tool: the language server then answers about the new code, " +
			"and reports the issues it finds in it, such as type errors, in any file. " +
			"These reports come in a <new-diagnostics> block at the end of a later tool result: use them to repair your edits.",
		Parameters: openai.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "string",
					"enum": []string{"goToDefinition", "findReferences"},
				},
				"filePath": map[string]any{
					"type":        "string",
					"description": "Absolute path, or a path relative to the agent's current working directory.",
				},
				"line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line number of the symbol.",
				},
				"character": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based character offset in UTF-16 code units (same as character count for ASCII).",
				},
			},
			"required": []string{"operation", "filePath", "line", "character"},
		},
		Run: func(_ context.Context, arguments string) (string, error) {
			var args struct {
				Operation string `json:"operation"`
				FilePath  string `json:"filePath"`
				Line      int    `json:"line"`
				Character int    `json:"character"`
			}
			if err := decodeArguments(arguments, &args); err != nil {
				return "", err
			}
			operation, supported := lspOperations[args.Operation]
			switch {
			case !supported:
				return "", fmt.Errorf("unsupported operation %q", args.Operation)
			case args.FilePath == "":
				return "", errors.New("filePath is required")
			case args.Line < 1 || args.Character < 1:
				return "", errors.New("line and character are counted from 1")
			}
			path, err := filepath.Abs(args.FilePath)
			if err != nil {
				return "", err
			}

			// The model counts lines and characters from one, like an editor,
			// and the protocol from zero.
			locations, err := operation.ask(server, path, lspPosition{Line: args.Line - 1, Character: args.Character - 1})
			if err != nil {
				return "", err
			}
			return operation.describe(locations), nil
		},
	}
}

// lspOperations holds what the LSP tool can do for the model, by the name of
// the operation: how it asks the server, and how it reports the locations
// that the server answers with.
var lspOperations = map[string]struct {
	ask      func(server *languageServer, path string, position lspPosition) ([]lspLocation, error)
	describe func(locations []lspLocation) string
}{
	"goToDefinition": {(*languageServer).definition, describeDefinitions},
	"findReferences": {(*languageServer).references, describeReferences},
}

// describeDefinitions reports where a symbol is defined: one line for each
// definition, of the form "Defined in pricing.py:1:5".
func describeDefinitions(locations []lspLocation) string {
	if len(locations) == 0 {
		return "No definition found"
	}
	lines := make([]string, len(locations))
	for i, location := range locations {
		file, line, character := startOf(location)
		lines[i] = fmt.Sprintf("Defined in %s:%d:%d", file, line, character)
	}
	return strings.Join(lines, "\n")
}

// describeReferences reports the references to a symbol, each of them on a
// line of its own under the file it is in:
//
//	Found 3 references across 2 files:
//
//	pricing.py:
//	  Line 1:5
//
//	checkout.py:
//	  Line 1:21
//	  Line 5:12
func describeReferences(locations []lspLocation) string {
	if len(locations) == 0 {
		return "No references found"
	}
	var files []string // in the order the server first mentions them
	positions := map[string][]string{}
	for _, location := range locations {
		file, line, character := startOf(location)
		if _, seen := positions[file]; !seen {
			files = append(files, file)
		}
		positions[file] = append(positions[file], fmt.Sprintf("  Line %d:%d", line, character))
	}

	sections := make([]string, len(files))
	for i, file := range files {
		sections[i] = file + ":\n" + strings.Join(positions[file], "\n")
	}
	return fmt.Sprintf("Found %s across %s:\n\n%s",
		count(len(locations), "reference"), count(len(files), "file"), strings.Join(sections, "\n\n"))
}

// count renders a number of things, as in "1 file" or "3 files".
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// startOf returns where a location starts, the way the model refers to code:
// the path of the file is relative to the working directory, and the line and
// the character are counted from one.
func startOf(location lspLocation) (file string, line, character int) {
	start := location.Range.Start
	return displayPath(pathFromURI(location.URI)), start.Line + 1, start.Character + 1
}

// fileURI returns the URI that identifies the file at an absolute path to the
// language server, such as file:///project/cart%20notes.py.
func fileURI(path string) string {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	return uri.String()
}

// pathFromURI returns the path of the file that a file URI identifies. Any
// other URI is returned as it is.
func pathFromURI(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return uri
	}
	return filepath.FromSlash(parsed.Path)
}

// displayPath returns the path of a file relative to the working directory if
// the file is inside it, and the path as it is otherwise.
func displayPath(path string) string {
	directory, err := os.Getwd()
	if err != nil {
		return path
	}
	if relative, inside := relativeTo(directory, path); inside {
		return relative
	}
	// The server and the agent may know the same file through different
	// symbolic links.
	if relative, inside := relativeTo(resolveSymlinks(directory), resolveSymlinks(path)); inside {
		return relative
	}
	return path
}

// relativeTo returns path relative to directory. inside reports whether path
// is in directory at all.
func relativeTo(directory, path string) (relative string, inside bool) {
	relative, err := filepath.Rel(directory, path)
	return relative, err == nil && filepath.IsLocal(relative)
}

// resolveSymlinks returns path with its symbolic links followed, or path
// itself if that cannot be done, as for a file that does not exist.
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
