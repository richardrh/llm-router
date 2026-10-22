package codex

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

func (m *Manager) adminProcess(ctx context.Context) (*process, error) {
	if m.closed {
		return nil, errors.New("codex manager closed")
	}
	if m.admin != nil {
		select {
		case <-m.admin.done:
			m.admin = nil
		default:
		}
	}
	if m.admin == nil {
		p, e := startProcess(ctx, m.command, m.cwd)
		if e != nil {
			return nil, e
		}
		m.admin = p
		go func() {
			for {
				select {
				case <-p.done:
					m.adminMu.Lock()
					if m.admin == p && !m.closed {
						m.snapshotMu.Lock()
						m.snapshot.Login = nil
						m.snapshot.Error = "Codex app-server exited; refresh the account or start sign-in again."
						m.snapshot.UpdatedAt = time.Now()
						m.snapshotMu.Unlock()
					}
					m.adminMu.Unlock()
					return
				case event := <-p.events:
					if len(event.ID) > 0 {
						_ = p.deny(event)
					}
					if event.Method == "account/login/completed" || event.Method == "account/updated" {
						m.adminMu.Lock()
						if m.admin == p && !m.closed {
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							_ = m.readAccount(ctx, p)
							cancel()
						}
						m.adminMu.Unlock()
					}
				}
			}
		}()
	}
	return m.admin, nil
}

// Snapshot returns cached state only: it never starts a process or performs RPCs.
func (m *Manager) Snapshot() Account {
	m.snapshotMu.RLock()
	defer m.snapshotMu.RUnlock()
	a := m.snapshot
	if a.Login != nil {
		l := *a.Login
		a.Login = &l
	}
	a.Models = append([]Model(nil), a.Models...)
	for i := range a.Models {
		a.Models[i].Input = append([]string(nil), a.Models[i].Input...)
		a.Models[i].ReasoningEfforts = append([]string(nil), a.Models[i].ReasoningEfforts...)
	}
	return a
}

func (m *Manager) saveError(err error) {
	if err != nil {
		m.snapshotMu.Lock()
		m.snapshot.Error = err.Error()
		m.snapshot.UpdatedAt = time.Now()
		m.snapshotMu.Unlock()
	}
}

// Refresh deliberately reloads idle authentication and the model catalog.
// A pending browser login keeps its original process and callback listener.
func (m *Manager) Refresh(ctx context.Context) (err error) {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	defer func() { m.saveError(err) }()
	if m.admin != nil {
		m.admin.loginMu.Lock()
		pending := m.admin.login != nil
		if m.admin.loginError != "" {
			m.loginError = m.admin.loginError
		}
		m.admin.loginMu.Unlock()
		if !pending {
			m.admin.close()
			m.admin = nil
		}
	}
	p, err := m.adminProcess(ctx)
	if err != nil {
		return err
	}
	if err = m.readAccount(ctx, p); err != nil {
		return err
	}
	models, err := m.models(ctx, p)
	if err != nil {
		return err
	}
	m.snapshotMu.Lock()
	m.snapshot.Models = models
	m.snapshot.UpdatedAt = time.Now()
	m.snapshotMu.Unlock()
	return nil
}

func (m *Manager) readAccount(ctx context.Context, p *process) error {
	var r struct {
		Account *struct{ Type, PlanType string }
	}
	if err := p.call(ctx, "account/read", map[string]any{"refreshToken": true}, &r); err != nil {
		m.saveError(err)
		return err
	}
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	a := &m.snapshot
	a.Type, a.Plan, a.SignedIn = "", "", false
	if r.Account != nil {
		a.Type, a.Plan = r.Account.Type, r.Account.PlanType
		a.SignedIn = a.Type == "chatgpt"
	}
	a.Login = nil
	if p.login != nil {
		l := *p.login
		a.Login = &l
	}
	if p.loginError != "" {
		m.loginError = p.loginError
	}
	a.Error = m.loginError
	a.CheckedAt, a.UpdatedAt = time.Now(), time.Now()
	return nil
}
func (m *Manager) Login(ctx context.Context) (_ Login, err error) {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	defer func() { m.saveError(err) }()
	p, e := m.adminProcess(ctx)
	if e != nil {
		return Login{}, e
	}
	p.loginMu.Lock()
	if p.login != nil {
		l := *p.login
		p.loginMu.Unlock()
		return l, nil
	}
	epoch := p.loginEpoch
	p.loginError = ""
	m.loginError = ""
	m.snapshotMu.Lock()
	m.snapshot.Error = ""
	m.snapshotMu.Unlock()
	p.loginMu.Unlock()
	var r struct {
		Type    string
		LoginID string
		AuthURL string
	}
	if e = p.call(ctx, "account/login/start", map[string]any{"type": "chatgpt"}, &r); e != nil {
		return Login{}, e
	}
	u, e := url.Parse(r.AuthURL)
	if e != nil || u.Scheme != "https" || u.User != nil || !(u.Hostname() == "openai.com" || strings.HasSuffix(u.Hostname(), ".openai.com") || u.Hostname() == "chatgpt.com" || strings.HasSuffix(u.Hostname(), ".chatgpt.com")) {
		return Login{}, errors.New("codex returned an invalid official login URL")
	}
	l := Login{ID: r.LoginID, URL: r.AuthURL}
	p.loginMu.Lock()
	if p.loginEpoch == epoch {
		p.login = &l
	}
	p.loginMu.Unlock()
	p.loginMu.Lock()
	m.snapshotMu.Lock()
	m.snapshot.Login = nil
	if p.login != nil {
		v := *p.login
		m.snapshot.Login = &v
	}
	p.loginMu.Unlock()
	m.snapshot.UpdatedAt = time.Now()
	m.snapshotMu.Unlock()
	if err := m.readAccount(ctx, p); err != nil {
		return l, err
	}
	return l, nil
}
func (m *Manager) CancelLogin(ctx context.Context, id string) (err error) {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	defer func() { m.saveError(err) }()
	p, e := m.adminProcess(ctx)
	if e != nil {
		return e
	}
	if e = p.call(ctx, "account/login/cancel", map[string]any{"loginId": id}, nil); e != nil {
		return e
	}
	p.loginMu.Lock()
	p.login = nil
	p.loginError = ""
	m.loginError = ""
	m.snapshotMu.Lock()
	m.snapshot.Login, m.snapshot.Error = nil, ""
	m.snapshot.UpdatedAt = time.Now()
	m.snapshotMu.Unlock()
	p.loginMu.Unlock()
	return nil
}
func (m *Manager) Logout(ctx context.Context) (err error) {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	defer func() { m.saveError(err) }()
	p, e := m.adminProcess(ctx)
	if e != nil {
		return e
	}
	if e = p.call(ctx, "account/logout", map[string]any{}, nil); e != nil {
		return e
	}
	p.loginMu.Lock()
	p.login, p.loginError = nil, ""
	p.loginMu.Unlock()
	m.loginError = ""
	m.snapshotMu.Lock()
	m.snapshot = Account{CheckedAt: time.Now(), UpdatedAt: time.Now()}
	m.snapshotMu.Unlock()
	m.mu.Lock()
	for key, s := range m.sessions {
		s.p.close()
		m.rememberLost(key, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	return nil
}
func (m *Manager) Models(ctx context.Context) ([]Model, error) {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	p, e := m.adminProcess(ctx)
	if e != nil {
		return nil, e
	}
	return m.models(ctx, p)
}

func (m *Manager) models(ctx context.Context, p *process) ([]Model, error) {
	var e error
	var models []Model
	cursor := ""
	for {
		var r struct {
			Data []struct {
				ID                        string
				Model                     string
				DisplayName               string
				IsDefault                 bool
				SupportedReasoningEfforts []struct{ ReasoningEffort string }
				InputModalities           []string
			}
			NextCursor *string
		}
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if e = p.call(ctx, "model/list", params, &r); e != nil {
			return nil, e
		}
		for _, v := range r.Data {
			id := v.Model
			if id == "" {
				id = v.ID
			}
			m := Model{ID: id, Name: v.DisplayName, Default: v.IsDefault, Input: v.InputModalities}
			for _, eff := range v.SupportedReasoningEfforts {
				m.ReasoningEfforts = append(m.ReasoningEfforts, eff.ReasoningEffort)
			}
			models = append(models, m)
		}
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		if *r.NextCursor == cursor {
			return nil, errors.New("codex model pagination did not advance")
		}
		cursor = *r.NextCursor
	}
	return models, nil
}
