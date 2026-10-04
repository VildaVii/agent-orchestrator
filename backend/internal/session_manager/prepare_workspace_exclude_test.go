package sessionmanager

import (
	"errors"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type excludingAgent struct {
	fakeAgent
	patterns []string
}

func (a excludingAgent) WorkspaceExcludePatterns() []string { return a.patterns }

func TestPrepareWorkspaceAppliesAgentExcludePatternsViaWorkspacePort(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker}
	ws := &fakeWorkspace{}
	m := New(Deps{Store: st, Workspace: ws})

	agent := excludingAgent{patterns: []string{"/.clinerules/zz-ao-managed-system-prompt.md"}}
	if err := m.prepareWorkspace(ctx, agent, "mer-1", "/ws/mer-1", "prompt", "", ports.AgentConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ws.excludePatterns, agent.patterns) {
		t.Fatalf("AddExclude patterns = %v, want %v", ws.excludePatterns, agent.patterns)
	}
	if !slices.Contains(ws.calls, "AddExclude:mer-1") {
		t.Fatalf("workspace calls = %v, want AddExclude for the session", ws.calls)
	}
}

func TestPrepareWorkspaceExcludeIsBestEffortAndOptional(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker}

	// An AddExclude failure is logged, not fatal: the adapter also writes a
	// sentinel .gitignore.
	failing := &fakeWorkspace{addExcludeErr: errors.New("boom")}
	m := New(Deps{Store: st, Workspace: failing})
	if err := m.prepareWorkspace(ctx, excludingAgent{patterns: []string{"/x"}}, "mer-1", "/ws/mer-1", "p", "", ports.AgentConfig{}, nil); err != nil {
		t.Fatalf("exclude failure must not fail prepare, got %v", err)
	}

	// Agents without the capability never touch the workspace port.
	plain := &fakeWorkspace{}
	m = New(Deps{Store: st, Workspace: plain})
	if err := m.prepareWorkspace(ctx, fakeAgent{}, "mer-1", "/ws/mer-1", "p", "", ports.AgentConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(plain.calls) != 0 {
		t.Fatalf("workspace calls = %v, want none", plain.calls)
	}
}
