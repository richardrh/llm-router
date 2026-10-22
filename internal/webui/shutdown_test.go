package webui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"llm-router/internal/inference"
)

func shutdownConversation(t *testing.T, h *Handler, cookie *http.Cookie) url.Values {
	t.Helper()
	page := consoleRequest(h, http.MethodGet, "/ui/playground", nil, cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("playground status = %d", page.Code)
	}
	match := regexp.MustCompile(`name="conversation_id"[^>]*value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("playground did not supply a conversation identity")
	}
	return url.Values{
		"csrf":            {csrfField(t, page.Body.String())},
		"conversation_id": {match[1]},
		"alias":           {"chat"},
		"prompt":          {"finish accounting before shutdown returns"},
	}
}

// Cancellation is not completion. Old conversations may still be unwinding
// after New or logout removes them from the current session map.
func TestConsoleCloseWaitsForAllInferenceWorkers(t *testing.T) {
	for _, removal := range []string{"active", "new conversation", "logout"} {
		t.Run(removal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan struct{}, 2)
				canceled := make(chan struct{}, 2)
				finished := make(chan struct{}, 2)
				release := make(chan struct{})
				var releaseOnce sync.Once
				h := New(Dependencies{
					GatewayKey: "gateway-secret",
					Snapshot: func() Status {
						return Status{Aliases: []Alias{{Name: "chat", Protocol: "openai-completions"}}}
					},
					Run: func(ctx context.Context, _ inference.Request, _ inference.Emit) (inference.Result, error) {
						started <- struct{}{}
						<-ctx.Done()
						canceled <- struct{}{}
						<-release
						// Represents the last accounting write before inference returns.
						finished <- struct{}{}
						return inference.Result{}, ctx.Err()
					},
				})
				defer func() {
					releaseOnce.Do(func() { close(release) })
					h.Close()
				}()

				cookie := consoleLogin(t, h)
				form := shutdownConversation(t, h, cookie)
				if got := consoleRequest(h, http.MethodPost, "/ui/playground/send", form, cookie); got.Code != http.StatusOK {
					t.Fatalf("send status = %d", got.Code)
				}
				<-started
				workers := 1
				if removal != "active" {
					path := "/ui/playground/new"
					wantStatus := http.StatusOK
					if removal == "logout" {
						path, wantStatus = "/ui/logout", http.StatusSeeOther
					}
					if got := consoleRequest(h, http.MethodPost, path, form, cookie); got.Code != wantStatus {
						t.Fatalf("%s status = %d", removal, got.Code)
					}
					<-canceled
					if removal == "logout" {
						cookie = consoleLogin(t, h)
					}
					form = shutdownConversation(t, h, cookie)
					if got := consoleRequest(h, http.MethodPost, "/ui/playground/send", form, cookie); got.Code != http.StatusOK {
						t.Fatalf("second send status = %d", got.Code)
					}
					<-started
					workers++
				}

				closed := make(chan struct{}, 2)
				go func() { h.Close(); closed <- struct{}{} }()
				synctest.Wait()
				<-canceled
				select {
				case <-closed:
					t.Fatal("Close returned while canceled inference could still record usage")
				default:
				}
				if got := consoleRequest(h, http.MethodPost, "/ui/playground/send", form, cookie); got.Code != http.StatusServiceUnavailable {
					t.Fatalf("shutdown accepted new work: status=%d", got.Code)
				}
				// Start the concurrent closer after synctest.Wait: a mutex
				// blocked inside sync.Once is not a durable synctest wait.
				go func() { h.Close(); closed <- struct{}{} }()
				releaseOnce.Do(func() { close(release) })
				for range workers {
					<-finished
				}
				for range 2 {
					<-closed
				}
			})
		})
	}
}

func TestConsoleShutdownRejectsRequestAlreadyPastHTTPAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		atSnapshot := make(chan struct{})
		releaseSnapshot := make(chan struct{})
		var delayed atomic.Bool
		var runs atomic.Int32
		h := New(Dependencies{
			GatewayKey: "gateway-secret",
			Snapshot: func() Status {
				if delayed.CompareAndSwap(true, false) {
					close(atSnapshot)
					<-releaseSnapshot
				}
				return Status{Aliases: []Alias{{Name: "chat", Protocol: "openai-completions"}}}
			},
			Run: func(context.Context, inference.Request, inference.Emit) (inference.Result, error) {
				runs.Add(1)
				return inference.Result{Text: "must not execute"}, nil
			},
		})
		defer h.Close()
		cookie := consoleLogin(t, h)
		form := shutdownConversation(t, h, cookie)
		delayed.Store(true)
		requestDone := make(chan struct{})
		go func() {
			defer close(requestDone)
			consoleRequest(h, http.MethodPost, "/ui/playground/send", form, cookie)
		}()
		<-atSnapshot
		h.Close()
		close(releaseSnapshot)
		<-requestDone
		synctest.Wait()
		if runs.Load() != 0 {
			t.Fatal("request admitted before shutdown started inference after Close")
		}
	})
}
