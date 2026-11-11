package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadToolReturnsExactFileContents(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{"single line", "print('apple')\n"},
		{"no trailing newline", "no newline at the end"},
		{"multiple lines", "line 1\nline 2\n\n  indented line 4\n"},
		{"unicode", "héllo wörld 🍎\n"},
		{"empty file", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, "apple.py", tt.contents)

			got, err := readTool.Run(context.Background(), jsonArgs(t, map[string]any{"file_path": path}))

			if err != nil {
				t.Fatalf("Read returned an error: %v", err)
			}
			if got != tt.contents {
				t.Errorf("Read = %q, want %q", got, tt.contents)
			}
		})
	}
}

func TestReadToolResolvesRelativePathsAgainstWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.txt"), "relative!")
	t.Chdir(dir)

	got, err := readTool.Run(context.Background(), `{"file_path": "notes.txt"}`)

	if err != nil {
		t.Fatalf("Read returned an error: %v", err)
	}
	if got != "relative!" {
		t.Errorf("Read = %q, want %q", got, "relative!")
	}
}

func TestReadToolRejectsBadCalls(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		wantErr   string
	}{
		{"malformed JSON", `{"file_path": `, "invalid arguments"},
		{"empty arguments", ``, "invalid arguments"},
		{"wrong argument type", `{"file_path": 42}`, "invalid arguments"},
		{"missing file_path", `{}`, "file_path is required"},
		{"empty file_path", `{"file_path": ""}`, "file_path is required"},
		{"file does not exist", jsonArgs(t, map[string]any{"file_path": filepath.Join(t.TempDir(), "missing.txt")}), "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readTool.Run(context.Background(), tt.arguments)

			if err == nil {
				t.Fatalf("Read = %q, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteToolCreatesFileWithExactContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"single line", "print('hello')\n"},
		{"no trailing newline", "no newline at the end"},
		{"multiple lines", "line 1\nline 2\n\n  indented line 4\n"},
		{"unicode", "héllo wörld 🍎\n"},
		{"empty content", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "new_file.py")

			result, err := writeTool.Run(context.Background(), jsonArgs(t, map[string]any{"file_path": path, "content": tt.content}))

			if err != nil {
				t.Fatalf("Write returned an error: %v", err)
			}
			if got := readFile(t, path); got != tt.content {
				t.Errorf("file contains %q, want %q", got, tt.content)
			}
			if !strings.Contains(result, path) {
				t.Errorf("result = %q, want it to name the written file %q", result, path)
			}
		})
	}
}

func TestWriteToolOverwritesExistingFile(t *testing.T) {
	path := writeTempFile(t, "notes.txt", "a much longer piece of old content\n")

	if _, err := writeTool.Run(context.Background(), jsonArgs(t, map[string]any{"file_path": path, "content": "new\n"})); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}

	if got := readFile(t, path); got != "new\n" {
		t.Errorf("file contains %q, want the old content fully replaced by %q", got, "new\n")
	}
}

func TestWriteToolCreatesMissingParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app", "utils", "helpers.py")

	if _, err := writeTool.Run(context.Background(), jsonArgs(t, map[string]any{"file_path": path, "content": "x = 1\n"})); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}

	if got := readFile(t, path); got != "x = 1\n" {
		t.Errorf("file contains %q, want %q", got, "x = 1\n")
	}
}

func TestWriteToolResolvesRelativePathsAgainstWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := writeTool.Run(context.Background(), `{"file_path": "app/main.py", "content": "relative!"}`); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "app", "main.py")); got != "relative!" {
		t.Errorf("file contains %q, want %q", got, "relative!")
	}
}

func TestWriteToolRejectsBadCalls(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name      string
		arguments string
		wantErr   string
	}{
		{"malformed JSON", `{"file_path": `, "invalid arguments"},
		{"empty arguments", ``, "invalid arguments"},
		{"wrong argument type", `{"file_path": "a.txt", "content": 42}`, "invalid arguments"},
		{"missing file_path", `{"content": "x"}`, "file_path is required"},
		{"empty file_path", `{"file_path": "", "content": "x"}`, "file_path is required"},
		{"missing content", jsonArgs(t, map[string]any{"file_path": filepath.Join(dir, "a.txt")}), "content is required"},
		{"null content", `{"file_path": "a.txt", "content": null}`, "content is required"},
		{"path is a directory", jsonArgs(t, map[string]any{"file_path": dir, "content": "x"}), "is a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := writeTool.Run(context.Background(), tt.arguments)

			if err == nil {
				t.Fatalf("Write = %q, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("rejected calls left %d files behind, want none", len(entries))
	}
}

func TestToolboxCallDispatchesByName(t *testing.T) {
	var gotArguments string
	toolbox := Toolbox{
		{Name: "Other", Run: func(context.Context, string) (string, error) {
			t.Error("the wrong tool was executed")
			return "", nil
		}},
		{Name: "Echo", Run: func(_ context.Context, arguments string) (string, error) {
			gotArguments = arguments
			return "echoed", nil
		}},
	}

	got, err := toolbox.Call(context.Background(), "Echo", `{"text": "hi"}`)

	if err != nil {
		t.Fatalf("Call returned an error: %v", err)
	}
	if got != "echoed" {
		t.Errorf("Call = %q, want %q", got, "echoed")
	}
	if gotArguments != `{"text": "hi"}` {
		t.Errorf("tool received arguments %q, want them passed through unchanged", gotArguments)
	}
}

func TestToolboxCallRejectsUnknownTool(t *testing.T) {
	_, err := baseToolbox(nil).Call(context.Background(), "Teleport", `{}`)

	if err == nil || !strings.Contains(err.Error(), `unknown tool "Teleport"`) {
		t.Errorf("error = %v, want it to report the unknown tool", err)
	}
}

// writeTempFile creates a file with the given contents in a fresh temporary
// directory and returns its absolute path.
func writeTempFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFile(t, path, contents)
	return path
}

// writeFile creates a file with the given contents, along with any missing
// parent directories.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFile returns the contents of a file that is expected to exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// jsonArgs encodes tool arguments the way the model sends them: as a JSON
// object serialized into a string.
func jsonArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
