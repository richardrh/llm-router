package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cliTestScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRunStreamsAndDecodesResult(t *testing.T) {
	script := cliTestScript(t, `
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}'
sleep 0.15
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Hello world","total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":900}}'
`)
	first := make(chan string, 1)
	done := make(chan struct{})
	var got cliRun
	var runErr error
	go func() {
		got, runErr = runCLICommand(context.Background(), []string{script}, func(text string) error {
			select {
			case first <- text:
			default:
			}
			return nil
		})
		close(done)
	}()
	select {
	case text := <-first:
		if text != "Hello" {
			t.Fatalf("first delta = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first delta was not delivered incrementally")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CLI did not finish")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if got.Text != "Hello world" || got.CostUSD != 0.0123 || !got.HasCost {
		t.Fatalf("run = %#v", got)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 20 || got.Usage.CacheReadTokens != 900 {
		t.Fatalf("usage = %#v", got.Usage)
	}
}

func TestCLIRunFailures(t *testing.T) {
	t.Run("nonzero includes stderr", func(t *testing.T) {
		script := cliTestScript(t, `printf 'something went wrong\n' >&2
exit 7`)
		_, err := runCLICommand(context.Background(), []string{script}, nil)
		if err == nil || !strings.Contains(err.Error(), "something went wrong") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("error result", func(t *testing.T) {
		script := cliTestScript(t, `printf '%s\n' '{"type":"result","is_error":true,"result":"nope"}'`)
		_, err := runCLICommand(context.Background(), []string{script}, nil)
		if err == nil || !strings.Contains(err.Error(), "error result") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancellation kills command", func(t *testing.T) {
		script := cliTestScript(t, `while :; do sleep 1; done`)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := runCLICommand(ctx, []string{script}, nil)
		if err != context.DeadlineExceeded {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCLIBuildCommand(t *testing.T) {
	got, err := buildCLICommand([]string{"claude", "--prompt", "{prompt}"}, "hello")
	if err != nil || len(got) != 3 || got[2] != "hello" {
		t.Fatalf("command = %#v, error = %v", got, err)
	}
	for _, argv := range [][]string{{"claude"}, {"claude", "{prompt}", "{prompt}"}, {"claude", "x{prompt}y", "{prompt}"}} {
		if _, err := buildCLICommand(argv, "hello"); err == nil {
			t.Errorf("buildCLICommand(%q) did not reject placeholder count", argv)
		}
	}
}

func TestCLIPromptContentForms(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Hello"},{"role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"image","text":"ignored"}]}]}`), &fields); err != nil {
		t.Fatal(err)
	}
	got, err := newCLIPrompt(fields)
	if err != nil {
		t.Fatal(err)
	}
	want := "Be concise\n\nUser: Hello\n\nAssistant: Hi"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	var empty map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"tool","content":"hidden"}]}`), &empty); err != nil {
		t.Fatal(err)
	}
	if _, err := newCLIPrompt(empty); err == nil {
		t.Fatal("expected no-text error")
	}
}
