package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	uri, err := s.open(path)
	if err != nil {
		return nil, err
	}
	result, err := s.client.Call("request", lspMessage{
		Method: "textDocument/definition",
		Params: map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     position,
		},
	})
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
		Name:        "LSP",
		Description: "Returns the file, line, and character where a symbol is defined.",
		Parameters: openai.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "string",
					"enum": []string{"goToDefinition"},
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
			switch {
			case args.Operation != "goToDefinition":
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
			locations, err := server.definition(path, lspPosition{Line: args.Line - 1, Character: args.Character - 1})
			if err != nil {
				return "", err
			}
			if len(locations) == 0 {
				return "No definition found", nil
			}
			lines := make([]string, len(locations))
			for i, location := range locations {
				lines[i] = "Defined in " + describeLocation(location)
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

// describeLocation renders where a location starts as path:line:character, the
// way the model refers to code: the path is relative to the working directory
// and the position is counted from one.
func describeLocation(location lspLocation) string {
	start := location.Range.Start
	return fmt.Sprintf("%s:%d:%d", displayPath(pathFromURI(location.URI)), start.Line+1, start.Character+1)
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
	// The server and the agent may know the same file through different
	// symbolic links.
	relative, err := filepath.Rel(resolveSymlinks(directory), resolveSymlinks(path))
	if err != nil || !filepath.IsLocal(relative) {
		return path
	}
	return relative
}

// resolveSymlinks returns path with its symbolic links followed, or path
// itself if that cannot be done, as for a file that does not exist.
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
