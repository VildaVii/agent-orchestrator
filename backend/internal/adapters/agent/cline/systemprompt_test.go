package cline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestMain stops git from discovering a repository above the OS temp dir, so
// t.TempDir() workspaces are never mistaken for part of a checkout that happens
// to contain the temp dir (AO installs write to the repo-wide info/exclude).
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	os.Exit(m.Run())
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// longSystemPrompt mimics a real AO prompt (~13 KB, multi-line, quotes and
// angle brackets), well past cmd.exe's 8191-character command-line limit.
func longSystemPrompt() string {
	line := "## AO Worker Role: run `ao report --done --note \"<text>\"` when finished.\n"
	return strings.Repeat(line, 180)
}

func TestLaunchAndRestoreArgvStayShortWithLongSystemPrompt(t *testing.T) {
	plugin := &Plugin{resolvedBinary: filepath.Join(t.TempDir(), "cline.cmd")}
	prompt := longSystemPrompt()
	if len(prompt) < 12000 {
		t.Fatalf("test prompt too short: %d", len(prompt))
	}

	launch, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Permissions:  ports.PermissionModeAuto,
		SystemPrompt: prompt,
		Config:       ports.AgentConfig{Model: "anthropic/claude-sonnet-4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	restore, ok, err := plugin.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Permissions:  ports.PermissionModeAuto,
		SystemPrompt: prompt,
		Session:      ports.SessionRef{Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "sess-1"}},
	})
	if err != nil || !ok {
		t.Fatalf("restore ok=%v err=%v", ok, err)
	}

	for name, argv := range map[string][]string{"launch": launch, "restore": restore} {
		if contains(argv, "-s") || contains(argv, "--system") {
			t.Fatalf("%s argv must not carry the system prompt inline: %#v", name, argv)
		}
		if got := len(strings.Join(argv, " ")); got >= 1000 {
			t.Fatalf("%s command line is %d chars, want well under cmd.exe's 8191 limit", name, got)
		}
	}
}

func TestGetAgentHooksWritesSystemPromptRule(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	prompt := longSystemPrompt()

	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: prompt}); err != nil {
		t.Fatal(err)
	}

	rulePath := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")
	data, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatalf("rule file missing: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, clineSystemPromptMarker+"\n") {
		t.Fatalf("rule file must start with the marker (never a YAML frontmatter fence), got %q", got[:40])
	}
	if !strings.Contains(got, strings.TrimRight(prompt, "\n")) || !strings.HasSuffix(got, "\n") {
		t.Fatal("rule file does not contain the full system prompt")
	}

	ignore, err := os.ReadFile(filepath.Join(ws, ".clinerules", ".gitignore"))
	if err != nil {
		t.Fatalf("rules .gitignore missing: %v", err)
	}
	if !strings.Contains(string(ignore), "/zz-ao-managed-system-prompt.md") {
		t.Fatalf("rule file not gitignored: %q", ignore)
	}
}

func TestGetAgentHooksRewritesSystemPromptRuleOnRestore(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	for _, prompt := range []string{"first prompt", "second prompt"} {
		if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "first prompt") || !strings.Contains(string(data), "second prompt") {
		t.Fatalf("rule file not refreshed: %q", data)
	}
}

func TestGetAgentHooksEmptySystemPromptRemovesManagedRule(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	rulePath := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")

	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rulePath); !os.IsNotExist(err) {
		t.Fatalf("stale managed rule should be removed, stat err = %v", err)
	}
}

func TestGetAgentHooksFailsLoudlyWhenClinerulesIsAFile(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	rules := filepath.Join(ws, ".clinerules")
	if err := os.WriteFile(rules, []byte("single-file rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "ao prompt"})
	if err == nil || !strings.Contains(err.Error(), "is a file") || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("err = %v, want a readable '.clinerules is a file' error", err)
	}
	if data, _ := os.ReadFile(rules); string(data) != "single-file rules\n" {
		t.Fatalf("user .clinerules file modified: %q", data)
	}
}

func TestGetAgentHooksFailsLoudlyOnUserAuthoredRuleWithSameName(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	rulePath := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(rulePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulePath, []byte("my own rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "ao prompt"})
	if err == nil || !strings.Contains(err.Error(), "not AO-managed") {
		t.Fatalf("err = %v, want error that the user file blocks AO's system prompt", err)
	}
	if data, rerr := os.ReadFile(rulePath); rerr != nil || string(data) != "my own rule\n" {
		t.Fatalf("user rule was modified or removed: %q, %v", data, rerr)
	}

	// With nothing to deliver there is nothing to lose: no error.
	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws}); err != nil {
		t.Fatalf("empty prompt with user rule = %v, want nil", err)
	}
}

func TestGetAgentHooksPreservesUserAuthoredRuleWithSameName(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	rulePath := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(rulePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulePath, []byte("my own rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Install is rejected (see TestGetAgentHooksFailsLoudly...); uninstall must
	// still leave the user's file alone.
	if err := plugin.UninstallHooks(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rulePath)
	if err != nil || string(data) != "my own rule\n" {
		t.Fatalf("user rule was modified or removed: %q, %v", data, err)
	}
}

func TestUninstallHooksRemovesSystemPromptRuleButKeepsOtherRules(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "ao prompt"}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(ws, ".clinerules", "team.md")
	if err := os.WriteFile(other, []byte("team rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := plugin.UninstallHooks(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("managed rule should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("user rule removed: %v", err)
	}
}

func TestIsManagedClineSystemPromptChecksFirstLineOnly(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "a.md")
	quoted := filepath.Join(dir, "b.md")
	if err := os.WriteFile(managed, []byte(clineSystemPromptMarker+"\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quoted, []byte("# my rule\n\nquoting "+clineSystemPromptMarker+" here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isManagedClineSystemPrompt(managed) {
		t.Fatal("marker on the first line (CRLF) must count as managed")
	}
	if isManagedClineSystemPrompt(quoted) {
		t.Fatal("marker quoted later in a user rule must not count as managed")
	}
}

// workspaceCleaner mirrors session_manager's optional agent capability.
type workspaceCleaner interface {
	CleanupWorkspace(ctx context.Context, cfg ports.WorkspaceHookConfig) error
}

var _ workspaceCleaner = (*Plugin)(nil)

func newLinkedWorktree(t *testing.T) (main, ws string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main = t.TempDir()
	runGit(t, main, "init", "-q")
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-q", "-m", "init")
	ws = filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", "--detach", ws)
	return main, ws
}

func TestCleanupWorkspacePreservesUserRuleAndMissingWorkspace(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	rulePath := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(rulePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulePath, []byte("my own rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plugin.CleanupWorkspace(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(rulePath); err != nil || string(data) != "my own rule\n" {
		t.Fatalf("user rule touched: %q, %v", data, err)
	}
	if err := plugin.CleanupWorkspace(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: filepath.Join(ws, "gone")}); err != nil {
		t.Fatalf("missing workspace must be a no-op, got %v", err)
	}
	if err := plugin.CleanupWorkspace(context.Background(), ports.WorkspaceHookConfig{}); err == nil {
		t.Fatal("empty WorkspacePath must be rejected")
	}
}

func TestSystemPromptRuleCarriesRoleScopeNotice(t *testing.T) {
	got := renderClineSystemPromptRule("body text")
	want := clineSystemPromptMarker + "\n\n" + clineRuleScopeNotice + "\n\nbody text\n"
	if got != want {
		t.Fatalf("rule = %q", got)
	}
	if !strings.Contains(clineRuleScopeNotice, "code reviewer") || !strings.Contains(clineRuleScopeNotice, "ignore everything below") {
		t.Fatal("scope notice must tell a reviewer to ignore the rule")
	}
}

func TestGetAgentHooksFillsRuleFromSystemPromptFileWhenInlineEmpty(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	file := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(file, []byte("prompt from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPromptFile: file}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md"))
	if err != nil || !strings.Contains(string(data), "prompt from file") {
		t.Fatalf("rule not filled from file: %q, %v", data, err)
	}

	// Inline text wins over the file; an unreadable file is an error, not a silent skip.
	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "inline wins", SystemPromptFile: file}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")); !strings.Contains(string(data), "inline wins") || strings.Contains(string(data), "prompt from file") {
		t.Fatalf("inline should win: %q", data)
	}
	err = plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: t.TempDir(), SystemPromptFile: filepath.Join(t.TempDir(), "missing.md")})
	if err == nil || !strings.Contains(err.Error(), "read system prompt file") {
		t.Fatalf("err = %v, want unreadable-file error", err)
	}
}

func TestWorkspaceExcludePatternsCoverRuleFileEvenWithUserOwnedGitignore(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	main, ws := newLinkedWorktree(t)
	// A repo-owned .clinerules/.gitignore without AO's sentinel: the sentinel
	// .gitignore is not written, so the rule file stays untracked ...
	rules := filepath.Join(ws, ".clinerules")
	if err := os.MkdirAll(rules, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rules, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plugin.GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "ao prompt"}); err != nil {
		t.Fatal(err)
	}
	if out := runGit(t, ws, "status", "--porcelain", "-uall"); !strings.Contains(out, "zz-ao-managed-system-prompt.md") {
		t.Fatalf("precondition: rule file should show as untracked without an exclude:\n%s", out)
	}

	// ... until session_manager applies the adapter's patterns through the
	// workspace port's AddExclude (appends to the common info/exclude).
	patterns := plugin.WorkspaceExcludePatterns()
	if len(patterns) != 1 || patterns[0] != "/.clinerules/zz-ao-managed-system-prompt.md" {
		t.Fatalf("patterns = %#v", patterns)
	}
	f, err := os.OpenFile(filepath.Join(main, ".git", "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(patterns[0] + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out := runGit(t, ws, "status", "--porcelain", "-uall"); strings.Contains(out, "zz-ao-managed-system-prompt.md") {
		t.Fatalf("rule file still untracked after the exclude pattern:\n%s", out)
	}
}

func TestCleanupWorkspaceRemovesRuleFile(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	cfg := ports.WorkspaceHookConfig{WorkspacePath: ws, SessionID: "s1", SystemPrompt: "ao prompt"}
	if err := plugin.GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := plugin.CleanupWorkspace(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("rule file should be removed, stat err = %v", err)
	}
	// Idempotent.
	if err := plugin.CleanupWorkspace(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestGetAgentHooksLeavesIdenticalRuleUntouchedOnRestore(t *testing.T) {
	plugin := &Plugin{resolvedBinary: "cline"}
	ws := t.TempDir()
	cfg := ports.WorkspaceHookConfig{WorkspacePath: ws, SystemPrompt: "same prompt"}
	if err := plugin.GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, ".clinerules", "zz-ao-managed-system-prompt.md")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := plugin.GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("identical rule must not be rewritten (mtime %v, want %v, err %v)", info.ModTime(), old, err)
	}
}
