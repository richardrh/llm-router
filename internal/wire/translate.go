package wire

import (
	"fmt"

	"github.com/richardrh/llm-router/internal/config"
)

type APIProtocol = config.APIProtocol

const (
	APIOpenAICompletions = config.APIOpenAICompletions
	APIOpenAIResponses   = config.APIOpenAIResponses
	APIAnthropicMessages = config.APIAnthropicMessages
)

// Translation between wire protocols.
//
// The router is a pass-through by default: an alias declares the wire its
// clients speak, and every target speaks the same one. A target may declare a
// different wire, and then the router converts in both directions — the request
// on the way out, and the response and its stream on the way back.
//
// Only the two pairs that matter in practice are bridged. Anything else is
// rejected at load time rather than half-supported.

// Translatable reports whether the router can bridge a client's wire to an
// upstream's.
func Translatable(client, upstream APIProtocol) bool {
	if client == upstream {
		return true
	}
	switch {
	case client == APIOpenAICompletions && upstream == APIAnthropicMessages:
		return true
	case client == APIAnthropicMessages && upstream == APIOpenAICompletions:
		return true
	}
	return false
}

// StreamTranslator converts one wire's SSE stream into another's. A nil
// translator means the stream is copied through untouched, which is the default
// and the only path that stays byte-transparent.
type StreamTranslator interface {
	// Write consumes upstream bytes and returns the bytes to send to the client.
	// It may return nothing while it accumulates a partial event.
	Write(p []byte) []byte
	// Close returns any bytes still owed once the upstream stream has ended.
	Close() []byte
}

// TranslateRequest converts a request body from the client's wire to the
// upstream's.
func TranslateRequest(client, upstream APIProtocol, body []byte) ([]byte, error) {
	switch {
	case client == upstream:
		return body, nil
	case client == APIOpenAICompletions && upstream == APIAnthropicMessages:
		return openAIToAnthropicRequest(body)
	case client == APIAnthropicMessages && upstream == APIOpenAICompletions:
		return anthropicToOpenAIRequest(body)
	}
	return nil, fmt.Errorf("no translation from %s to %s", client, upstream)
}

// TranslateResponse converts a complete, non-streaming response body from the
// upstream's wire back to the client's, reporting the alias in place of the
// upstream's model id.
func TranslateResponse(client, upstream APIProtocol, body []byte, alias string) ([]byte, error) {
	switch {
	case client == upstream:
		return body, nil
	case client == APIOpenAICompletions && upstream == APIAnthropicMessages:
		return anthropicToOpenAIResponse(body, alias)
	case client == APIAnthropicMessages && upstream == APIOpenAICompletions:
		return openAIToAnthropicResponse(body, alias)
	}
	return nil, fmt.Errorf("no translation from %s to %s", upstream, client)
}

// NewStreamTranslator returns the translator for a stream, or nil when the
// client and the upstream share a wire and the stream can be relayed untouched.
func NewStreamTranslator(client, upstream APIProtocol, alias string) StreamTranslator {
	switch {
	case client == upstream:
		return nil
	case client == APIOpenAICompletions && upstream == APIAnthropicMessages:
		return newAnthropicToOpenAIStream(alias)
	case client == APIAnthropicMessages && upstream == APIOpenAICompletions:
		return newOpenAIToAnthropicStream(alias)
	}
	return nil
}

// errTranslationFailed is written when a response cannot be converted. The status
// line is already committed by then, so the failure has to be reported in the
// body; the client is told the shape it asked for, and the operator gets a log
// line.
var ErrTranslationFailed = []byte(`{"error":{"message":"the upstream response could not be translated","type":"router_translation_error"}}`)
