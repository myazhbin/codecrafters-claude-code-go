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
	"strings"

	"github.com/openai/openai-go/v3"
)

// languageServer is the agent's side of the conversation with a language
// server: on top of the transport, it keeps track of what the server has been
// told about the project's files.
type languageServer struct {
	client *LSPClient
	// opened holds the URIs of the documents sent to the server. The server
	// needs a document before it can answer about it, and rejects a document
	// that is sent twice.
	opened map[string]bool
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
	return &languageServer{client: client, opened: map[string]bool{}}
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

// open sends the document at path to the server, unless the server has it
// already, and returns the URI that identifies the document.
func (s *languageServer) open(path string) (uri string, err error) {
	uri = fileURI(path)
	if s.opened[uri] {
		return uri, nil
	}
	language, known := s.client.ExtensionToLanguage[filepath.Ext(path)]
	if !known {
		return "", fmt.Errorf("no language server is configured for files like %s", filepath.Base(path))
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	_, err = s.client.Call("notify", lspMessage{
		Method: "textDocument/didOpen",
		Params: map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": language,
				"version":    1,
				"text":       string(text),
			},
		},
	})
	if err != nil {
		return "", err
	}
	s.opened[uri] = true
	return uri, nil
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
			"such as the calls of a function, including through aliases.",
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
