package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeLLM is an OpenAI-compatible chat completions endpoint for tests. It
// answers each request with the next scripted reply and records every request
// it receives, so tests can assert on what the program sent to the model.
type fakeLLM struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	replies  []llmReply
	requests []llmRequest
}

// llmReply is one scripted response of the fake LLM.
type llmReply struct {
	status  int            // HTTP status to respond with; 0 means 200 OK
	message map[string]any // returned as choices[0].message when status is 200
}

// llmRequest is one request received by the fake LLM.
type llmRequest struct {
	Method        string
	Path          string
	Authorization string
	Body          map[string]any
}

// newFakeLLM starts a fake LLM that responds with the given replies, in order.
// The server is shut down when the test ends.
func newFakeLLM(t *testing.T, replies ...llmReply) *fakeLLM {
	t.Helper()
	llm := &fakeLLM{t: t, replies: replies}
	llm.server = httptest.NewServer(http.HandlerFunc(llm.handle))
	t.Cleanup(llm.server.Close)
	return llm
}

// assistantText scripts a plain-text assistant message.
func assistantText(content string) llmReply {
	return llmReply{message: map[string]any{"role": "assistant", "content": content}}
}

// toolCall is one function call requested by a scripted assistant message.
type toolCall struct {
	id        string
	name      string
	arguments string // JSON-encoded, exactly as the model would send it
}

// wire returns the tool call as it appears in the API's JSON.
func (c toolCall) wire() map[string]any {
	return map[string]any{
		"id":   c.id,
		"type": "function",
		"function": map[string]any{
			"name":      c.name,
			"arguments": c.arguments,
		},
	}
}

// assistantToolCalls scripts an assistant message that requests tool calls.
func assistantToolCalls(calls ...toolCall) llmReply {
	toolCalls := make([]any, len(calls))
	for i, call := range calls {
		toolCalls[i] = call.wire()
	}
	return llmReply{message: map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": toolCalls,
	}}
}

// withContent returns a copy of the reply whose message also carries text, as
// models often narrate what they are about to do alongside their tool calls.
func (r llmReply) withContent(content string) llmReply {
	message := make(map[string]any, len(r.message))
	for key, value := range r.message {
		message[key] = value
	}
	message["content"] = content
	r.message = message
	return r
}

// httpError scripts a failed request with the given HTTP status.
func httpError(status int) llmReply {
	return llmReply{status: status}
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("fakeLLM: reading request body: %v", err)
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("fakeLLM: request body is not a JSON object: %v\n%s", err, raw)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, llmRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		Body:          body,
	})
	var reply *llmReply
	if n := len(f.requests); n <= len(f.replies) {
		reply = &f.replies[n-1]
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if reply == nil {
		// 400 is not retried by the SDK, so the program fails fast.
		f.t.Errorf("fakeLLM: unexpected request #%d: no scripted reply left", len(f.requests))
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"fakeLLM: no scripted reply left"}}`)
		return
	}
	if reply.status != 0 && reply.status != http.StatusOK {
		w.WriteHeader(reply.status)
		io.WriteString(w, `{"error":{"message":"fakeLLM: scripted failure"}}`)
		return
	}

	finishReason := "stop"
	if _, ok := reply.message["tool_calls"]; ok {
		finishReason = "tool_calls"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": 0,
		"model":   body["model"],
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": finishReason,
			"message":       reply.message,
		}},
	})
}

// recorded returns the requests received so far.
func (f *fakeLLM) recorded() []llmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llmRequest(nil), f.requests...)
}

// onlyRequest returns the single request the fake LLM received, failing the
// test if it received any other number of requests.
func (f *fakeLLM) onlyRequest() llmRequest {
	f.t.Helper()
	requests := f.recorded()
	if len(requests) != 1 {
		f.t.Fatalf("LLM received %d requests, want exactly 1", len(requests))
	}
	return requests[0]
}

// runCLI runs the program against the fake LLM, as if invoked with args, and
// returns what it wrote to stdout and stderr along with its exit code.
func runCLI(t *testing.T, llm *fakeLLM, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIWithEnv(t, map[string]string{
		"OPENROUTER_API_KEY":  "test-key",
		"OPENROUTER_BASE_URL": llm.server.URL,
	}, args...)
}

// runCLIWithEnv is like runCLI but with full control over the environment.
func runCLIWithEnv(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(key string) string { return env[key] }
	code = run(context.Background(), args, getenv, &out, &errOut)
	return out.String(), errOut.String(), code
}

// The helpers below build the messages a request is expected to carry, in the
// generic form that request bodies are decoded into.

func userMessage(content string) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

// assistantMessage is the echo of an assistant reply. Like the API client, it
// leaves out the content when there is none.
func assistantMessage(content string, calls ...toolCall) map[string]any {
	message := map[string]any{"role": "assistant"}
	if content != "" {
		message["content"] = content
	}
	if len(calls) > 0 {
		toolCalls := make([]any, len(calls))
		for i, call := range calls {
			toolCalls[i] = call.wire()
		}
		message["tool_calls"] = toolCalls
	}
	return message
}

func toolMessage(toolCallID, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": toolCallID, "content": content}
}

// decodeJSON parses a JSON document into its generic Go representation.
func decodeJSON(t *testing.T, document string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		t.Fatalf("invalid JSON in test: %v\n%s", err, document)
	}
	return value
}

// toJSON renders a value as indented JSON for failure messages.
func toJSON(t *testing.T, value any) string {
	t.Helper()
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("cannot marshal %#v: %v", value, err)
	}
	return string(out)
}
