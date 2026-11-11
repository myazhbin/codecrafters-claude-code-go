package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/openai/openai-go/v3"
)

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
