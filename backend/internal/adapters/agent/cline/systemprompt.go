package cline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AO's standing instructions reach Cline as a workspace rule file rather than
// as `-s <prompt>` on the command line. The prompt is ~13 KB; Cline on Windows
// is launched through the npm `cline.cmd` shim, so it runs under `cmd.exe /c`,
// whose 8191-character command-line limit makes a launch with the inline prompt
// fail with "The command line is too long." before Cline ever starts.
//
// Cline loads every `.md`/`.markdown`/`.txt` file directly inside the
// workspace `.clinerules/` directory and appends it to the system prompt under
// a `# Rules` section named after the file (subdirectories such as `hooks/`
// are not scanned). Unlike `-s`, which replaces Cline's built-in system prompt,
// rules are added on top of it, so Cline keeps its own tool guidance.
//
// Source (verified on Cline CLI 3.0.68, npm/node_modules/cline/node_modules/
// @cline): @cline/core dist/index.js, rule discovery `Xc()` takes `.clinerules`
// as a file or a directory and reads a directory with one non-recursive readdir
// (files only, extensions .md/.markdown/.txt, plus AGENTS.md); @cline/shared
// dist/storage/paths.d.ts and storage/index.js define the `.clinerules`
// location. Rule files may start with YAML frontmatter and must have a body.
const (
	// clineSystemPromptFileName is the rule file AO owns. Its stem is the rule
	// name Cline shows in the `# Rules` section. The name is deliberately unlikely
	// to clash with a user rule and sorts alphabetically last (directory listings are
	// normally name-ordered, so AO's instructions tend to follow the project's own rules).
	clineSystemPromptFileName = "zz-ao-managed-system-prompt.md"

	// clineSystemPromptMarker tags the AO-generated rule file so install and
	// uninstall can tell it from a user-authored rule of the same name. It is an
	// HTML comment, invisible when rendered and harmless in the model's context.
	// It also guarantees the file never starts with a `---` line, which Cline
	// would parse as YAML frontmatter.
	clineSystemPromptMarker = "<!-- ao-managed-cline-system-prompt -->"
)

func clineRulesDir(workspacePath string) string {
	return filepath.Join(workspacePath, clineHooksDirName)
}

func clineSystemPromptPath(workspacePath string) string {
	return filepath.Join(clineRulesDir(workspacePath), clineSystemPromptFileName)
}

// clineRuleScopeNotice opens the rule file. Cline loads every rule in the
// workspace for every run there, so an AO code reviewer started in the same
// checkout (Cline has no per-run switch to disable rules) would otherwise read
// the worker's instructions as its own; this gates the rule on the role.
const clineRuleScopeNotice = "This rule applies only to the AO worker session of this checkout. If your system prompt says you are an AO code reviewer (or any other role), ignore everything below this line."

func renderClineSystemPromptRule(systemPrompt string) string {
	return clineSystemPromptMarker + "\n\n" + clineRuleScopeNotice + "\n\n" + strings.TrimRight(systemPrompt, "\n") + "\n"
}

// clineSystemPromptText resolves the prompt text the same way the other
// file-based adapters (copilot, muse, crush) do: the inline SystemPrompt wins,
// and AO's SystemPromptFile (written by session_manager.prepareSystemPromptFile,
// carrying the same text) is the fallback.
func clineSystemPromptText(inline, file string) (string, error) {
	if strings.TrimSpace(inline) != "" {
		return strings.TrimRight(inline, "\n"), nil
	}
	if strings.TrimSpace(file) == "" {
		return "", nil
	}
	data, err := os.ReadFile(file) //nolint:gosec // path is AO-owned launch config
	if err != nil {
		return "", fmt.Errorf("cline: read system prompt file: %w", err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// checkRulesDirUsable fails with a readable error when `.clinerules` exists as
// a regular file (Cline's single-file rules form): AO needs a directory for its
// hook scripts and system prompt rule, and would otherwise surface an opaque
// "not a directory" error from MkdirAll.
func checkRulesDirUsable(workspacePath string) error {
	rulesDir := clineRulesDir(workspacePath)
	info, err := os.Stat(rulesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", rulesDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file, but AO needs %s to be a directory to install its Cline hooks and system prompt rule; move its content into a file inside that directory (e.g. %s%crules.md)", rulesDir, clineHooksDirName, clineHooksDirName, filepath.Separator)
	}
	return nil
}

// installSystemPromptRule writes AO's system prompt into the workspace-local
// `.clinerules/zz-ao-managed-system-prompt.md` rule file and keeps it out of git. An empty
// prompt removes a previously written AO-owned file. A user-authored file at
// that path (no marker) is never overwritten, but it is an error: silently
// continuing would start the session without AO's system prompt.
func installSystemPromptRule(workspacePath, inlinePrompt, promptFile string) error {
	systemPrompt, err := clineSystemPromptText(inlinePrompt, promptFile)
	if err != nil {
		return err
	}
	path := clineSystemPromptPath(workspacePath)
	if hookutil.FileExists(path) && !isManagedClineSystemPrompt(path) {
		if strings.TrimSpace(systemPrompt) == "" {
			return nil // nothing to deliver, nothing lost
		}
		return fmt.Errorf("%s exists and is not AO-managed (no %s marker); AO will not overwrite it, but without it the session would start without AO's system prompt - rename or remove that file", path, clineSystemPromptMarker)
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return removeSystemPromptRule(workspacePath)
	}

	rulesDir := clineRulesDir(workspacePath)
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		return fmt.Errorf("create rules dir: %w", err)
	}
	// A restore re-runs the install with the same prompt: leave an identical
	// file alone (no mtime churn, no write race with a running Cline).
	content := []byte(renderClineSystemPromptRule(systemPrompt))
	if existing, err := os.ReadFile(path); err != nil || !bytes.Equal(existing, content) { //nolint:gosec // path built from caller-owned workspace dir
		if err := hookutil.AtomicWriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", clineSystemPromptFileName, err)
		}
	}
	// Same rule as the hook scripts: only a file AO actually wrote is ignored, so
	// a user-authored rule keeps counting as dirt and survives workspace teardown.
	if err := hookutil.EnsureWorkspaceGitignore(rulesDir, clineSystemPromptFileName); err != nil {
		return fmt.Errorf("gitignore: %w", err)
	}
	return nil
}

// systemPromptExcludePattern is the git ignore pattern for the rule file.
const systemPromptExcludePattern = "/" + clineHooksDirName + "/" + clineSystemPromptFileName

// WorkspaceExcludePatterns lists the workspace files this adapter writes that
// must stay out of git status. session_manager applies them through the
// workspace port's AddExclude (the one place AO writes git excludes), which
// covers the case the `.clinerules/.gitignore` written next to the rule file
// cannot: a repo-owned `.clinerules/.gitignore` without AO's sentinel.
func (p *Plugin) WorkspaceExcludePatterns() []string {
	return []string{systemPromptExcludePattern}
}

// removeSystemPromptRule deletes the AO-owned rule file, if present. A missing
// file or a user-authored file at that path is a no-op.
func removeSystemPromptRule(workspacePath string) error {
	path := clineSystemPromptPath(workspacePath)
	if !hookutil.FileExists(path) || !isManagedClineSystemPrompt(path) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", clineSystemPromptFileName, err)
	}
	return nil
}

func isManagedClineSystemPrompt(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path built from caller-owned workspace dir
	if err != nil {
		return false
	}
	// Only the first line counts: a user rule that merely quotes the marker
	// further down must not be mistaken for AO's file and overwritten.
	first, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(first) == clineSystemPromptMarker
}

// CleanupWorkspace removes the AO-owned system prompt rule while the workspace
// still exists. session_manager calls it AFTER the workspace was destroyed on
// normal teardown (then the directory is gone and this is a no-op), so it only
// has an effect on the paths where the checkout survives: spawn rollback, agent
// switch and preserved worktrees. The git exclude pattern
// (WorkspaceExcludePatterns) is intentionally not removed: the workspace port
// has no remove operation, and one idempotent, AO-named line in the local
// info/exclude (never committed) is the accepted cost, like the attachments
// pattern. Hook scripts are left in place as before (gitignored, refreshed on
// the next install).
func (p *Plugin) CleanupWorkspace(ctx context.Context, cfg ports.WorkspaceHookConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.WorkspacePath) == "" {
		return errors.New("cline.CleanupWorkspace: WorkspacePath is required")
	}
	if _, err := os.Stat(cfg.WorkspacePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cline.CleanupWorkspace: %w", err)
	}
	if err := removeSystemPromptRule(cfg.WorkspacePath); err != nil {
		return fmt.Errorf("cline.CleanupWorkspace: %w", err)
	}
	return nil
}
