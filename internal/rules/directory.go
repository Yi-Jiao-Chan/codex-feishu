// Package rules evaluates canonical directories and notification policy.
package rules

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Scope struct {
	CWD, ProjectRoot, ProjectCWD, GitCommonDir string
	Allowed, Worktree                          bool
	Reason                                     string
}
type DirectoryRule struct {
	Path      string
	Recursive bool
}

func CanonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("directory must be absolute")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", errors.New("directory unavailable")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New("directory unavailable")
	}
	return real, nil
}

func Inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ResolveScope authorizes either a physical allowed directory, or the matching
// relative directory in a registered worktree of the same Git repository. Merely
// sharing a common-dir string does not authorize an unregistered checkout.
func ResolveScope(ctx context.Context, cwd string, allowedRoots []string) (Scope, error) {
	actual, err := CanonicalDirectory(cwd)
	if err != nil {
		return Scope{Reason: "cwd_unavailable"}, err
	}
	scope := Scope{CWD: actual, Reason: "outside_allowed_roots"}
	actualGit, actualGitOK := readGit(ctx, actual)
	for _, configured := range allowedRoots {
		allowed, err := CanonicalDirectory(configured)
		if err != nil {
			return scope, err
		}
		if Inside(allowed, actual) {
			scope.Allowed, scope.Reason = true, ""
			scope.ProjectRoot, scope.ProjectCWD = actual, actual
			if actualGitOK {
				scope.ProjectRoot, scope.GitCommonDir = actualGit.primary, actualGit.common
				rel, _ := filepath.Rel(actualGit.root, actual)
				scope.ProjectCWD = filepath.Join(actualGit.primary, rel)
				scope.Worktree = actualGit.root != actualGit.primary
			}
			return scope, nil
		}
		if !actualGitOK {
			continue
		}
		allowedGit, ok := readGit(ctx, allowed)
		if !ok || actualGit.common != allowedGit.common {
			continue
		}
		// Restrict a configured project subdirectory to that same subdirectory
		// in the other worktree. Allowing the repository alone would widen it.
		candidateRel, _ := filepath.Rel(actualGit.root, actual)
		mapped := filepath.Join(allowedGit.root, candidateRel)
		if !Inside(allowed, mapped) {
			continue
		}
		scope.Allowed, scope.Worktree, scope.Reason = true, actualGit.root != actualGit.primary, ""
		scope.ProjectRoot, scope.GitCommonDir = actualGit.primary, actualGit.common
		scope.ProjectCWD = filepath.Join(actualGit.primary, candidateRel)
		return scope, nil
	}
	return scope, nil
}

// Each path is already canonical and proven by ResolveScope. Map the actual
// worktree cwd to the original project cwd without widening exact to recursive.
func Muted(scope Scope, rules []DirectoryRule) (DirectoryRule, bool) {
	for _, rule := range rules {
		for _, path := range []string{scope.CWD, scope.ProjectCWD} {
			if path != "" && (path == rule.Path || rule.Recursive && Inside(rule.Path, path)) {
				return rule, true
			}
		}
	}
	return DirectoryRule{}, false
}

type gitDirectory struct{ root, common, primary string }

func readGit(ctx context.Context, cwd string) (gitDirectory, bool) {
	root, err := gitValue(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitDirectory{}, false
	}
	root, err = CanonicalDirectory(root)
	if err != nil {
		return gitDirectory{}, false
	}
	common, err := gitValue(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return gitDirectory{}, false
	}
	common, err = CanonicalDirectory(common)
	if err != nil {
		return gitDirectory{}, false
	}
	list, err := gitValue(ctx, cwd, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return gitDirectory{}, false
	}
	// A bare repository has no original working directory to map to.
	for _, field := range strings.Split(strings.Split(list, "\x00\x00")[0], "\x00") {
		if field == "bare" {
			return gitDirectory{}, false
		}
	}
	var primary string
	registered := false
	for _, field := range strings.Split(list, "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		path, err := CanonicalDirectory(strings.TrimPrefix(field, "worktree "))
		if err != nil {
			continue
		}
		if primary == "" {
			primary = path
		}
		if path == root {
			registered = true
		}
	}
	if !registered || primary == "" {
		return gitDirectory{}, false
	}
	return gitDirectory{root: root, common: common, primary: primary}, true
}

func gitValue(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = cwd
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("git metadata unavailable")
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
