package creation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"golang.org/x/sys/unix"
)

var (
	ErrDirectoryUnavailable  = errors.New("creation directory is unavailable or not readable, writable and searchable")
	ErrDirectoryOutsideRoots = errors.New("creation directory is outside allowed roots")
	ErrDirectoryProject      = errors.New("creation project does not match the selected directory")
	ErrDirectoryLimit        = errors.New("creation directory listing exceeds the candidate limit; enter an absolute path")
	ErrDirectoryCursor       = errors.New("invalid creation directory cursor")
)

const maxDirectoryCandidates = 4096

type DirectoryService struct {
	Roots, Common []string
	Recent        []string
	Projects      func(context.Context) ([]codexclient.Project, error)
}

type DirectoryChoice struct {
	Path, ProjectID, Label string
	ReadOnly               bool
}
type DirectoryPage struct {
	Path, Parent, Next string
	Choices            []DirectoryChoice
	ReadOnly           bool
	Total, Offset      int
}

func physicalDirectory(path string) (string, error) {
	if strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", ErrDirectoryUnavailable
	}
	real, err := rules.CanonicalDirectory(path)
	if err != nil || unix.Access(real, unix.R_OK|unix.X_OK) != nil {
		return "", ErrDirectoryUnavailable
	}
	return real, nil
}

func (s DirectoryService) allowed(path string) bool {
	for _, root := range s.Roots {
		real, err := rules.CanonicalDirectory(root)
		if err == nil && rules.Inside(real, path) {
			return true
		}
	}
	return false
}

// Browse requires read/search access and an authorized physical path. ReadOnly
// marks directories that can be browsed but cannot be selected for execution.
func (s DirectoryService) Browse(ctx context.Context, path string) (DirectoryChoice, error) {
	if err := ctx.Err(); err != nil {
		return DirectoryChoice{}, err
	}
	real, err := physicalDirectory(path)
	if err != nil {
		return DirectoryChoice{}, err
	}
	if !s.allowed(real) {
		return DirectoryChoice{}, ErrDirectoryOutsideRoots
	}
	return DirectoryChoice{Path: real, Label: filepath.Base(real), ReadOnly: unix.Access(real, unix.W_OK) != nil}, nil
}

func projectForPath(projects []codexclient.Project, path, id string) (string, string) {
	for _, project := range projects {
		if project.ID == "" || id != "" && project.ID != id {
			continue
		}
		for _, root := range project.Roots {
			real, err := rules.CanonicalDirectory(root.Path)
			if err == nil && real == path {
				return project.ID, project.Name
			}
		}
	}
	return "", ""
}

// Validate rechecks physical scope and access at selection/dispatch time. An
// explicit project must match an actual project root exactly; neither matching
// names nor parent-directory similarity establishes project assignment.
func (s DirectoryService) Validate(ctx context.Context, path, projectID string) (DirectoryChoice, error) {
	choice, err := s.Browse(ctx, path)
	if err != nil {
		return choice, err
	}
	if choice.ReadOnly {
		return DirectoryChoice{}, ErrDirectoryUnavailable
	}
	if projectID == "" {
		return choice, nil
	}
	if s.Projects == nil {
		return DirectoryChoice{}, ErrDirectoryProject
	}
	projects, err := s.Projects(ctx)
	if err != nil {
		return DirectoryChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return DirectoryChoice{}, err
	}
	id, label := projectForPath(projects, choice.Path, projectID)
	if id == "" {
		return DirectoryChoice{}, ErrDirectoryProject
	}
	choice.ProjectID = id
	if label != "" {
		choice.Label = label
	}
	return choice, nil
}

// Default never falls back to the service process cwd or an arbitrary project.
// An unavailable recent directory is followed only by configured common paths.
func (s DirectoryService) Default(ctx context.Context, recent string) (DirectoryChoice, error) {
	for _, candidate := range append([]string{recent}, s.Common...) {
		if err := ctx.Err(); err != nil {
			return DirectoryChoice{}, err
		}
		if candidate == "" {
			continue
		}
		choice, err := s.Validate(ctx, candidate, "")
		if err == nil {
			return choice, nil
		}
	}
	return DirectoryChoice{}, ctx.Err()
}

// availableProjects enriches browsable paths when the native catalog is online.
// Filesystem navigation remains available without it; explicit project claims
// still use Validate, which fails closed when the catalog cannot be verified.
func (s DirectoryService) availableProjects(ctx context.Context) ([]codexclient.Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Projects == nil {
		return nil, nil
	}
	projects, err := s.Projects(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, nil
	}
	return projects, nil
}

func (s DirectoryService) knownCandidates(projects []codexclient.Project) ([]string, error) {
	if len(s.Recent)+len(s.Common) > maxDirectoryCandidates {
		return nil, ErrDirectoryLimit
	}
	candidates := append(append([]string{}, s.Recent...), s.Common...)
	for _, project := range projects {
		for _, root := range project.Roots {
			candidates = append(candidates, root.Path)
			if len(candidates) > maxDirectoryCandidates {
				return nil, ErrDirectoryLimit
			}
		}
	}
	return candidates, nil
}

func directoryLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, 100)
}

func validDirectoryCursor(after string) bool {
	return after == "" || filepath.IsAbs(after) && filepath.Clean(after) == after && strings.IndexFunc(after, unicode.IsControl) < 0
}

func directoryProject(choice DirectoryChoice, projects []codexclient.Project) DirectoryChoice {
	if id, name := projectForPath(projects, choice.Path, ""); id != "" {
		choice.ProjectID = id
		if name != "" {
			choice.Label = name
		}
	}
	return choice
}

// Quick offers selectable known paths, preserving recent/common/catalog order.
// It does not enumerate configured roots or discover filesystem descendants.
func (s DirectoryService) Quick(ctx context.Context, limit int) ([]DirectoryChoice, error) {
	projects, err := s.availableProjects(ctx)
	if err != nil {
		return nil, err
	}
	candidates, err := s.knownCandidates(projects)
	if err != nil {
		return nil, err
	}
	choices := []DirectoryChoice{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		choice, err := s.Browse(ctx, candidate)
		if err != nil || choice.ReadOnly || seen[choice.Path] {
			continue
		}
		seen[choice.Path] = true
		choices = append(choices, directoryProject(choice, projects))
		if len(choices) == min(directoryLimit(limit), 20) {
			break
		}
	}
	return choices, ctx.Err()
}

// Search is explicit and bounded: text filters known paths; an absolute path
// either names one directory or filters the immediate entries of its parent.
// It never recursively scans the machine. Selection still requires Validate.
func (s DirectoryService) Search(ctx context.Context, query, after string, limit int) (DirectoryPage, error) {
	page := DirectoryPage{Choices: []DirectoryChoice{}}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if !validDirectoryCursor(after) {
		return page, ErrDirectoryCursor
	}
	if strings.IndexFunc(query, unicode.IsControl) >= 0 {
		return page, ErrDirectoryUnavailable
	}
	if filepath.IsAbs(query) {
		choice, err := s.Browse(ctx, query)
		if err == nil {
			projects, err := s.availableProjects(ctx)
			if err != nil {
				return page, err
			}
			page.Path, page.ReadOnly = choice.Path, choice.ReadOnly
			if up, err := s.Browse(ctx, filepath.Dir(choice.Path)); err == nil && up.Path != choice.Path {
				page.Parent = up.Path
			}
			if choice.Path > after {
				page.Choices = append(page.Choices, directoryProject(choice, projects))
			} else {
				page.Offset = 1
			}
			page.Total = 1
			return page, ctx.Err()
		}
		// Only a genuinely missing final component is treated as a prefix.
		// Existing but inaccessible paths, files, and scope escapes fail closed.
		if _, statErr := os.Stat(query); !errors.Is(statErr, os.ErrNotExist) {
			return page, err
		}
		prefix := strings.ToLower(filepath.Base(filepath.Clean(query)))
		return s.listDirectories(ctx, filepath.Dir(filepath.Clean(query)), after, limit, func(name string) bool {
			return strings.HasPrefix(strings.ToLower(name), prefix)
		})
	}
	projects, err := s.availableProjects(ctx)
	if err != nil {
		return page, err
	}
	candidates, err := s.knownCandidates(projects)
	if err != nil {
		return page, err
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	unique := map[string]DirectoryChoice{}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return DirectoryPage{}, err
		}
		choice, err := s.Browse(ctx, candidate)
		if err != nil {
			continue
		}
		choice = directoryProject(choice, projects)
		if strings.Contains(strings.ToLower(choice.Label), needle) || strings.Contains(strings.ToLower(choice.Path), needle) {
			unique[choice.Path] = choice
		}
	}
	page.Choices, page.Next, page.Total, page.Offset = directoryChoicesPage(unique, after, limit)
	return page, ctx.Err()
}

func directoryChildren(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrDirectoryUnavailable
	}
	defer f.Close()
	entries, err := f.ReadDir(maxDirectoryCandidates + 1)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, ErrDirectoryUnavailable
	}
	if len(entries) > maxDirectoryCandidates {
		return nil, ErrDirectoryLimit
	}
	candidates := []string{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			candidates = append(candidates, filepath.Join(path, entry.Name()))
		}
	}
	return candidates, nil
}

func directoryChoicesPage(unique map[string]DirectoryChoice, after string, limit int) ([]DirectoryChoice, string, int, int) {
	limit = directoryLimit(limit)
	paths := make([]string, 0, len(unique))
	for path := range unique {
		if path > after {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	choices := []DirectoryChoice{}
	for _, path := range paths[:min(limit, len(paths))] {
		choices = append(choices, unique[path])
	}
	next := ""
	if len(paths) > limit {
		next = paths[limit-1]
	}
	return choices, next, len(unique), len(unique) - len(paths)
}

// List reads configured/project roots, or one directory's immediate entries.
// It never recurses. Next is the last canonical path of a full page and is only
// a lexical cursor, never an authorization claim or an executable directory.
func (s DirectoryService) List(ctx context.Context, parent, after string, limit int) (DirectoryPage, error) {
	return s.ListMatching(ctx, parent, "", after, limit)
}

// ListMatching filters immediate directory names before pagination. The match
// uses the visible entry basename, including an in-scope symlink's name, while
// returned choices and cursors always use the physical canonical path.
func (s DirectoryService) ListMatching(ctx context.Context, parent, filter, after string, limit int) (DirectoryPage, error) {
	needle := strings.ToLower(filter)
	return s.listDirectories(ctx, parent, after, limit, func(name string) bool {
		return strings.Contains(strings.ToLower(name), needle)
	})
}

func (s DirectoryService) listDirectories(ctx context.Context, parent, after string, limit int, matches func(string) bool) (DirectoryPage, error) {
	if err := ctx.Err(); err != nil {
		return DirectoryPage{}, err
	}
	if !validDirectoryCursor(after) {
		return DirectoryPage{}, ErrDirectoryCursor
	}
	page := DirectoryPage{Choices: []DirectoryChoice{}}
	var projects []codexclient.Project
	var err error
	var candidates []string
	if parent == "" {
		projects, err = s.availableProjects(ctx)
		if err != nil {
			return page, err
		}
		for _, project := range projects {
			for _, root := range project.Roots {
				candidates = append(candidates, root.Path)
				if len(candidates) > maxDirectoryCandidates {
					return page, ErrDirectoryLimit
				}
			}
		}
		if len(candidates)+len(s.Common)+len(s.Roots) > maxDirectoryCandidates {
			return page, ErrDirectoryLimit
		}
		candidates = append(candidates, s.Common...)
		candidates = append(candidates, s.Roots...)
	} else {
		choice, err := s.Browse(ctx, parent)
		if err != nil {
			return page, err
		}
		page.Path = choice.Path
		page.ReadOnly = choice.ReadOnly
		up := filepath.Dir(choice.Path)
		if up != choice.Path {
			if parent, err := s.Browse(ctx, up); err == nil {
				page.Parent = parent.Path
			}
		}
		candidates, err = directoryChildren(ctx, choice.Path)
		if err != nil {
			return page, err
		}
	}
	if len(candidates) > maxDirectoryCandidates {
		return page, ErrDirectoryLimit
	}
	unique := map[string]DirectoryChoice{}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return DirectoryPage{}, err
		}
		if !matches(filepath.Base(candidate)) {
			continue
		}
		choice, err := s.Browse(ctx, candidate)
		if err != nil {
			continue
		}
		if _, exists := unique[choice.Path]; exists {
			continue
		}
		unique[choice.Path] = directoryProject(choice, projects)
	}
	page.Choices, page.Next, page.Total, page.Offset = directoryChoicesPage(unique, after, limit)
	return page, ctx.Err()
}
