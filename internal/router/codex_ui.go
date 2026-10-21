package router

import (
	"context"
	"fmt"
	"llm-router/internal/webui"
)

func (s *Server) uiConnections() []webui.Connection {
	out := make([]webui.Connection, 0, len(s.router.upstreams))
	for _, name := range s.cfg.UpstreamsInOrder() {
		upstream := s.router.upstreams[name]
		connection := webui.Connection{Name: name, Kind: string(upstream.cfg.KindValue()), SignedIn: upstream.key != ""}
		if upstream.codex != nil {
			account := upstream.codex.Snapshot()
			connection.CheckedAt, connection.UpdatedAt = account.CheckedAt, account.UpdatedAt
			connection.AccountType, connection.Plan, connection.SignedIn, connection.Error = account.Type, account.Plan, account.SignedIn, account.Error
			if account.Login != nil {
				connection.LoginID, connection.LoginURL, connection.UserCode = account.Login.ID, account.Login.URL, account.Login.UserCode
			}
			for _, model := range account.Models {
				connection.Models = append(connection.Models, webui.ConnectionModel{ID: model.ID, Name: model.Name, Default: model.Default})
			}
		}
		out = append(out, connection)
	}
	return out
}

func (s *Server) uiConnect(ctx context.Context, action webui.ConnectionAction) error {
	upstream := s.router.upstreams[action.Upstream]
	if upstream == nil || upstream.codex == nil {
		return fmt.Errorf("choose a configured Codex upstream")
	}
	switch action.Kind {
	case webui.ConnectionRefresh:
		return upstream.codex.Refresh(ctx)
	case webui.ConnectionLogin:
		_, err := upstream.codex.Login(ctx)
		return err
	case webui.ConnectionCancel:
		if action.LoginID == "" {
			return fmt.Errorf("login ID is required")
		}
		return upstream.codex.CancelLogin(ctx, action.LoginID)
	case webui.ConnectionLogout:
		return upstream.codex.Logout(ctx)
	default:
		return fmt.Errorf("unsupported provider authentication action")
	}
}
