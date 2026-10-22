package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexAuthProtocolHelper(t *testing.T) {
	if os.Getenv("ROUTER_CODEX_AUTH_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	if marker := os.Getenv("ROUTER_CODEX_ASYNC_LOGIN_MARKER"); marker != "" {
		go func() {
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if os.Getenv("ROUTER_CODEX_EXIT_ON_MARKER") == "1" {
				os.Exit(0)
			}
			send(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login-test", "success": true}})
			send(map[string]any{"method": "account/updated", "params": map[string]any{"authMode": "chatgpt"}})
		}()
	}
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		_ = json.Unmarshal(scanner.Bytes(), &req)
		if log := os.Getenv("ROUTER_CODEX_RPC_LOG"); log != "" {
			f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(2)
			}
			_, _ = fmt.Fprintln(f, req.Method)
			_ = f.Close()
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
		case "initialized":
			continue
		case "account/login/start":
			result = map[string]any{"type": "chatgpt", "loginId": "login-test", "authUrl": "https://auth.openai.com/oauth/authorize?state=not-a-real-login"}
		case "account/login/cancel":
			result = map[string]any{"status": "canceled"}
		case "account/logout":
		case "account/read":
			result = map[string]any{"account": nil, "requiresOpenaiAuth": true}
			if marker := os.Getenv("ROUTER_CODEX_ASYNC_LOGIN_MARKER"); marker != "" {
				if _, err := os.Stat(marker); err == nil {
					result = map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}}
				}
			}
		case "model/list":
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		default:
			send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
			continue
		}
		send(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}
func TestManagedBrowserLoginRemainsPendingAcrossAccountRefresh(t *testing.T) {
	t.Setenv("ROUTER_CODEX_AUTH_HELPER", "1")
	m := New([]string{os.Args[0], "-test.run=^TestCodexAuthProtocolHelper$", "--"}, "", 2, time.Minute)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	login, e := m.Login(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if login.ID != "login-test" || login.URL != "https://auth.openai.com/oauth/authorize?state=not-a-real-login" {
		t.Fatalf("unexpected login %+v", login)
	}
	if e = m.Refresh(ctx); e != nil {
		t.Fatal(e)
	}
	account := m.Snapshot()
	if account.SignedIn || account.Login == nil || account.Login.ID != "login-test" {
		t.Fatalf("pending login lost %+v", account)
	}
	if e = m.CancelLogin(ctx, login.ID); e != nil {
		t.Fatal(e)
	}
	account = m.Snapshot()
	if account.Login != nil || account.SignedIn {
		t.Fatalf("cancel did not clear login %+v", account)
	}
	if e = m.Logout(ctx); e != nil {
		t.Fatal(e)
	}
}

func TestSnapshotsNeverStartOrRefetch(t *testing.T) {
	t.Setenv("ROUTER_CODEX_AUTH_HELPER", "1")
	log := filepath.Join(t.TempDir(), "rpc")
	t.Setenv("ROUTER_CODEX_RPC_LOG", log)
	m := New([]string{os.Args[0], "-test.run=^TestCodexAuthProtocolHelper$", "--"}, "", 2, time.Minute)
	defer m.Close()
	for range 10 {
		a := m.Snapshot()
		if !a.CheckedAt.IsZero() || a.SignedIn {
			t.Fatalf("cold account not unknown: %+v", a)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("cold snapshot started a process: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if m.Snapshot().CheckedAt.IsZero() {
			t.Fatal("refresh not cached")
		}
	}
	after, err := os.ReadFile(log)
	if err != nil || string(before) != string(after) {
		t.Fatalf("snapshot performed RPCs: %q -> %q (%v)", before, after, err)
	}
}

func TestLoginNotificationsUpdateSnapshotsWithoutRefresh(t *testing.T) {
	t.Setenv("ROUTER_CODEX_AUTH_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "complete")
	t.Setenv("ROUTER_CODEX_ASYNC_LOGIN_MARKER", marker)
	m := New([]string{os.Args[0], "-test.run=^TestCodexAuthProtocolHelper$", "--"}, "", 2, time.Minute)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Login(ctx); err != nil {
		t.Fatal(err)
	}
	a := m.Snapshot()
	if a.Login == nil {
		t.Fatal("login not pending")
	}
	a.Login.ID = "tampered"
	if m.Snapshot().Login.ID != "login-test" {
		t.Fatal("snapshot aliases pending flow")
	}
	if err := os.WriteFile(marker, []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		a = m.Snapshot()
		if a.SignedIn && a.Login == nil && a.Plan == "plus" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("completion notification not reflected: %+v", a)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRefreshFailureRemainsVisibleInSnapshots(t *testing.T) {
	m := New([]string{filepath.Join(t.TempDir(), "missing-codex")}, "", 1, time.Minute)
	defer m.Close()
	err := m.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected process start failure")
	}
	for range 3 {
		a := m.Snapshot()
		if a.Error != err.Error() || a.UpdatedAt.IsZero() || !a.CheckedAt.IsZero() {
			t.Fatalf("failed refresh state not preserved: %+v", a)
		}
	}
}

func TestPendingLoginSnapshotClearsWhenProcessExits(t *testing.T) {
	t.Setenv("ROUTER_CODEX_AUTH_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "exit")
	t.Setenv("ROUTER_CODEX_ASYNC_LOGIN_MARKER", marker)
	t.Setenv("ROUTER_CODEX_EXIT_ON_MARKER", "1")
	m := New([]string{os.Args[0], "-test.run=^TestCodexAuthProtocolHelper$", "--"}, "", 1, time.Minute)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := m.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Login == nil {
		t.Fatal("login was not pending")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		snapshot := m.Snapshot()
		if snapshot.Login == nil && snapshot.Error != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("dead login process remained pending in the read-only snapshot")
		case <-time.After(time.Millisecond):
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := m.Snapshot(); snapshot.Error != "" || snapshot.CheckedAt.IsZero() {
		t.Fatalf("successful refresh did not clear the transport failure: %+v", snapshot)
	}
}
