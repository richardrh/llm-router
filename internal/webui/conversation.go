package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"llm-router/internal/inference"
)

type ConversationState uint8

const (
	Ready ConversationState = iota
	Running
	AwaitingTools
	Stopped
	Failed
	Closed
)

type conversation struct {
	id, alias            string
	state                ConversationState
	messages             []MessageView
	history              []inference.Message
	activeText           strings.Builder
	activeStart          int
	err, upstream, model string
	cancel               context.CancelFunc
	revision             uint64
	turn                 uint64
}

func newConversation() *conversation {
	var id [24]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return &conversation{id: hex.EncodeToString(id[:]), state: Ready}
}

// transition is the sole state mutation point. Terminal conversations cannot resume.
func (c *conversation) transition(next ConversationState, message string) bool {
	allowed := next == Closed || (c.state == Ready && next == Running) ||
		(c.state == Running && (next == Ready || next == AwaitingTools || next == Stopped || next == Failed))
	if c.state == Closed || !allowed {
		return false
	}
	c.state = next
	c.err = message
	c.revision++
	if next != Running && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return true
}

func (v PlayView) IsRunning() bool {
	return v.State == Running
}

func (v PlayView) CanSend() bool {
	return v.State == Ready
}

func (v PlayView) AwaitingTools() bool {
	return v.State == AwaitingTools
}

func (v PlayView) HistoryMessages() []MessageView {
	if v.ActiveStart >= 0 && v.ActiveStart <= len(v.Messages) {
		return v.Messages[:v.ActiveStart]
	}
	return v.Messages
}

func (v PlayView) ActiveMessages() []MessageView {
	if v.ActiveStart >= 0 && v.ActiveStart <= len(v.Messages) {
		return v.Messages[v.ActiveStart:]
	}
	return nil
}
