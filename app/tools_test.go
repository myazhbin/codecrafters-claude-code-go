package main

import (
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

			got, err := readTool.Run(jsonArgs(t, map[string]any{"file_path": path}))

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
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("relative!"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got, err := readTool.Run(`{"file_path": "notes.txt"}`)

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
			got, err := readTool.Run(tt.arguments)

			if err == nil {
				t.Fatalf("Read = %q, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestToolboxCallDispatchesByName(t *testing.T) {
	var gotArguments string
	toolbox := Toolbox{
		{Name: "Other", Run: func(string) (string, error) {
			t.Error("the wrong tool was executed")
			return "", nil
		}},
		{Name: "Echo", Run: func(arguments string) (string, error) {
			gotArguments = arguments
			return "echoed", nil
		}},
	}

	got, err := toolbox.Call("Echo", `{"text": "hi"}`)

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
	_, err := defaultToolbox().Call("Teleport", `{}`)

	if err == nil || !strings.Contains(err.Error(), `unknown tool "Teleport"`) {
		t.Errorf("error = %v, want it to report the unknown tool", err)
	}
}

// writeTempFile creates a file with the given contents in a fresh temporary
// directory and returns its absolute path.
func writeTempFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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
