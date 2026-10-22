package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/inference"
)

const maxPlayBytes = 1 << 20
const maxPromptBytes = 32 << 10

func (h *Handler) closePlayground(s *browserSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.play != nil {
		s.play.transition(Closed, "Session closed. Sign in again.")
	}
	s.play = nil
}

// playViewLocked captures revision, state, and messages in one critical section.
func (h *Handler) playViewLocked(s *browserSession, activeOnly bool) PlayView {
	if s.closed {
		return PlayView{CSRF: s.csrf, State: Closed, Error: "Session closed. Sign in again."}
	}
	if s.play == nil {
		s.play = newConversation()
	}
	c := s.play
	first := 0
	if activeOnly {
		first = c.activeStart
	}
	messages := append([]MessageView(nil), c.messages[first:]...)
	redact := func(text string) string {
		if h.deps.GatewayKey != "" {
			return strings.ReplaceAll(text, h.deps.GatewayKey, "[redacted]")
		}
		return text
	}
	for i := range messages {
		messages[i].Text = redact(messages[i].Text)
	}
	return PlayView{CSRF: s.csrf, ConversationID: c.id, Alias: c.alias, Messages: messages,
		State: c.state, Revision: c.revision, ActiveStart: c.activeStart - first, Error: redact(c.err), Upstream: c.upstream,
		UpstreamModel: c.model, StreamURL: "/ui/playground/stream?conversation_id=" + c.id}
}

func (h *Handler) playView(s *browserSession) PlayView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return h.playViewLocked(s, false)
}

func (h *Handler) servePlayground(w http.ResponseWriter, r *http.Request, s *browserSession) {
	if r.URL.Path == "/ui/playground/stream" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.playStream(w, r, s)
		return
	}
	switch r.URL.Path {
	case "/ui/playground":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	case "/ui/playground/new", "/ui/playground/stop":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		if !s.closed && s.play != nil && r.FormValue("conversation_id") == s.play.id {
			if strings.HasSuffix(r.URL.Path, "/new") {
				s.play.transition(Closed, "")
				s.play = newConversation()
			} else {
				s.play.transition(Stopped, "Stopped; start a New conversation to continue.")
			}
		}
		s.mu.Unlock()
	case "/ui/playground/send":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.playSend(r, s)
	default:
		http.NotFound(w, r)
		return
	}
	view := h.playView(s)
	view.Aliases = h.deps.Snapshot().Aliases
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		_ = Playground(view).Render(r.Context(), w)
	} else {
		_ = Page("Playground", s.csrf, "playground", Playground(view)).Render(r.Context(), w)
	}
}

func (h *Handler) playSend(r *http.Request, s *browserSession) {
	prompt, alias := r.FormValue("prompt"), r.FormValue("alias")
	status := h.deps.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.play == nil {
		s.play = newConversation()
	}
	c := s.play
	if r.FormValue("conversation_id") != c.id || c.state == Running {
		return
	}
	reject := func(message string) {
		c.err = message
		c.revision++
	}
	if c.state != Ready {
		reject("This conversation cannot continue. Start a New conversation.")
		return
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > maxPromptBytes {
		reject("Enter a prompt of at most 32 KiB.")
		return
	}
	if len(c.history) >= 100 {
		reject("Conversation limit reached; start a New conversation.")
		return
	}
	if c.alias != "" && alias != c.alias {
		reject("The alias is locked for this conversation.")
		return
	}
	var selected *Alias
	for i := range status.Aliases {
		if status.Aliases[i].Name == alias {
			selected = &status.Aliases[i]
			break
		}
	}
	if selected == nil || !supportedAlias(*selected) {
		reject("Choose a supported chat or messages alias.")
		return
	}
	if selected.HasCLI && r.FormValue("acknowledge_cli") != "on" {
		reject("Acknowledge CLI execution before sending.")
		return
	}
	total := len(prompt)
	for _, m := range c.messages {
		total += len(m.Text)
	}
	if total > maxPlayBytes/2 {
		reject("Conversation size limit reached; start a New conversation.")
		return
	}
	if h.deps.Run == nil {
		reject("Inference is unavailable.")
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.workers.Add(1)
	h.mu.Unlock()

	c.alias = alias
	c.history = append(c.history, inference.Message{Role: "user", Content: prompt})
	c.activeStart = len(c.messages)
	c.activeText.Reset()
	c.upstream, c.model = "", ""
	c.messages = append(c.messages, MessageView{Role: "user", Text: prompt}, MessageView{Role: "assistant"})
	c.turn++
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.transition(Running, "")
	req := inference.Request{Model: alias, Protocol: config.APIProtocol(selected.Protocol), ConversationID: c.id,
		Messages: append([]inference.Message(nil), c.history...), Stream: !selected.ClientTools, MaxOutputTokens: selected.MaxOutputTokens}
	go h.runPlayground(ctx, s, c, c.turn, req)
}

func (h *Handler) runPlayground(ctx context.Context, s *browserSession, c *conversation, turn uint64, req inference.Request) {
	defer h.workers.Done()
	emit := func(event inference.Event) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || s.play != c || c.state != Running || c.turn != turn {
			return context.Canceled
		}
		switch event.Kind {
		case inference.RouteSelected:
			c.upstream, c.model = event.Route.Upstream, event.Route.Model
		case inference.TextDelta:
			m := &c.messages[len(c.messages)-1]
			if len(m.Text)+len(event.Text) > inference.MaxResponseBytes {
				c.transition(Failed, "Response size limit reached; start a New conversation.")
				return fmt.Errorf("response size limit reached")
			}
			c.activeText.WriteString(event.Text)
			m.Text = c.activeText.String()
		}
		c.revision++
		return nil
	}
	result, err := h.deps.Run(ctx, req, emit)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.play != c || c.state != Running || c.turn != turn {
		return
	}
	if len(result.Text) > inference.MaxResponseBytes {
		c.transition(Failed, "Response size limit reached; start a New conversation.")
		return
	}
	c.upstream, c.model = result.Route.Upstream, result.Route.Model
	if err == nil || result.Text != "" {
		c.messages[len(c.messages)-1].Text = result.Text
	}
	if err != nil {
		c.transition(Failed, err.Error()+"; start a New conversation.")
		return
	}
	if len(result.ToolCalls) > 0 {
		tools, _ := json.Marshal(result.ToolCalls)
		c.messages = append(c.messages, MessageView{Role: "tool calls (pending external harness)", Text: string(tools)})
	}
	if result.HasUsage {
		usage, _ := json.Marshal(result.Usage)
		c.messages = append(c.messages, MessageView{Role: "usage", Text: string(usage)})
	}
	if len(result.ToolCalls) > 0 {
		c.transition(AwaitingTools, "Tool calls require an external harness. Start a New conversation to continue.")
	} else {
		c.history = append(c.history, inference.Message{Role: "assistant", Content: result.Text})
		c.transition(Ready, "")
	}
}

func (h *Handler) playStream(w http.ResponseWriter, r *http.Request, s *browserSession) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", http.StatusInternalServerError)
		return
	}
	id := r.URL.Query().Get("conversation_id")
	s.mu.Lock()
	valid := !s.closed && s.play != nil && s.play.id == id
	s.mu.Unlock()
	if !valid {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	revision := ^uint64(0)
	initial := true
	for {
		s.mu.Lock()
		valid := !s.closed && s.play != nil && s.play.id == id
		var view PlayView
		state := Closed
		changed := false
		if valid {
			state = s.play.state
			changed = initial || revision != s.play.revision
			if changed {
				view = h.playViewLocked(s, !initial)
			}
		}
		s.mu.Unlock()
		if !valid {
			_, _ = fmt.Fprint(w, "event: done\ndata: done\n\n")
			flusher.Flush()
			return
		}
		if changed {
			var fragment bytes.Buffer
			event := "active"
			if initial {
				event = "history"
				if err := Conversation(view).Render(r.Context(), &fragment); err != nil {
					return
				}
			} else if err := ActiveTurn(view).Render(r.Context(), &fragment); err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
				return
			}
			for _, line := range strings.Split(fragment.String(), "\n") {
				if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
					return
				}
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
			revision = view.Revision
			initial = false
		}
		if state != Running {
			_, _ = fmt.Fprint(w, "event: done\ndata: done\n\n")
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
