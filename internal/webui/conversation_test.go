package webui

import "testing"

func TestConversationTransitions(t *testing.T) {
	for _, terminal := range []ConversationState{Ready, AwaitingTools, Stopped, Failed, Closed} {
		c := newConversation()
		if c.transition(AwaitingTools, "") || c.transition(Failed, "") || c.transition(Stopped, "") {
			t.Fatal("ready conversation accepted invalid transition")
		}
		if !c.transition(Running, "") || !c.transition(terminal, "") {
			t.Fatalf("running transition to %v rejected", terminal)
		}
		if terminal == Ready {
			if !c.transition(Running, "") {
				t.Fatal("completed conversation cannot continue")
			}
		} else if c.transition(Running, "") {
			t.Fatal("terminal conversation resumed")
		}
		if terminal != Closed && !c.transition(Closed, "") {
			t.Fatal("conversation did not close")
		}
		if c.transition(Ready, "") || c.transition(Running, "") {
			t.Fatal("closed conversation resumed")
		}
	}
}

func TestPlaygroundCloseDoesNotRevokeAuthentication(t *testing.T) {
	s := &browserSession{play: newConversation()}
	h := &Handler{}
	h.closePlayground(s)
	if s.closed || s.play != nil {
		t.Fatal("playground closure must drop conversation without revoking auth")
	}
}
