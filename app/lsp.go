package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	// reported holds the issues of the latest report taken in for each file,
	// by URI: what the model is taken to know about the file.
	reported map[string][]diagnostic
	// pending holds the issues that the model has yet to be told about.
	pending []fileIssue
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
	err := s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": s.client.ExtensionToLanguage[filepath.Ext(path)],
			"version":    1,
			"text":       text,
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
	err := s.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(document.path), "version": version},
		"contentChanges": []any{map[string]any{"text": text}},
	})
	if err != nil {
		return err
	}
	document.version, document.text = version, text
	return nil
}

// notify sends the server a notification about a document. The reports that
// the server has made so far are taken in first: they are about the documents
// as the server has them now, before the notification changes that.
func (s *languageServer) notify(method string, params map[string]any) error {
	s.collectDiagnostics()
	_, err := s.client.Call("notify", lspMessage{Method: method, Params: params})
	return err
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
