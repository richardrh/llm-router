package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestUnsupportedControlsExposeClientErrorSentinel(t *testing.T) {
	m := New([]string{"must-not-run"}, "", 1, 0)
	defer m.Close()
	for _, control := range []string{"max_tokens", "max_completion_tokens", "temperature", "parallel_tool_calls", "unknown_execution_control"} {
		t.Run(control, func(t *testing.T) {
			fields := lifecycleFields()
			fields[control] = json.RawMessage(`1`)
			_, err := m.Run(context.Background(), "invalid", "model", fields, nil)
			if !errors.Is(err, ErrUnsupportedRequest) {
				t.Fatalf("expected unsupported request sentinel, got %v", err)
			}
		})
	}
	fields := lifecycleFields()
	fields["tool_choice"] = json.RawMessage(`"none"`)
	if _, err := m.Run(context.Background(), "invalid", "model", fields, nil); !errors.Is(err, ErrUnsupportedRequest) {
		t.Fatalf("tool choice classification: %v", err)
	}
}

func TestUnsupportedMultimodalContentExposesClientErrorSentinel(t *testing.T) {
	m := lifecycleManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fields := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]`)}
	if _, err := m.Run(ctx, "image", "model", fields, nil); !errors.Is(err, ErrUnsupportedRequest) {
		t.Fatalf("content classification: %v", err)
	}
}

func TestTransportStreamingFieldsRemainAccepted(t *testing.T) {
	m := lifecycleManager(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fields := lifecycleFields()
	fields["stream"] = json.RawMessage(`true`)
	fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	if _, err := m.Run(ctx, "stream", "model", fields, nil); err != nil {
		t.Fatal(err)
	}
}
