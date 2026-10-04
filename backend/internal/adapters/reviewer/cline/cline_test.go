package cline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewCommandUsesSystemPromptWithoutAutoApproval(t *testing.T) {
	root := t.TempDir()
	system := filepath.Join(root, "system.md")
	if err := os.WriteFile(system, []byte("review only"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Reviewer{resolveBinary: func(context.Context) (string, error) { return "/opt/cline", nil }}
	spec, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{TaskPromptRoot: root, SystemPromptFile: system, Prompt: "read task"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/cline", "-s", "review only" + workerRuleNotice}; !slices.Equal(spec.Argv, want) || spec.InitialMessage != "read task" {
		t.Fatalf("spec = %+v", spec)
	}
	// The reviewer shares the worker checkout, so it must be told that the
	// worker's .clinerules prompt is not its own (Cline cannot disable rules).
	if !strings.Contains(spec.Argv[2], "zz-ao-managed-system-prompt") || !strings.Contains(spec.Argv[2], "Ignore any worker role") {
		t.Fatalf("reviewer prompt lacks the worker-rule override: %q", spec.Argv[2])
	}
	for _, forbidden := range []string{"--auto-approve", "--yolo"} {
		if slices.Contains(spec.Argv, forbidden) {
			t.Fatalf("argv enables %q: %#v", forbidden, spec.Argv)
		}
	}
}
