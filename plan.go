package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Plan operations.
const (
	opKeep   = "keep"
	opDelete = "delete"
	opExec   = "exec"
)

// Action is one line of the plan: something to keep, something to delete, or
// something to hand to an external tool.
type Action struct {
	Rule    string   `json:"rule"`
	Op      string   `json:"op"`
	Path    string   `json:"path,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`
	Command []string `json:"command,omitempty"`

	directory *plannedDirectory
	fileInfo  os.FileInfo
}

// Plan is the full set of actions before anything is touched.
type Plan struct {
	Actions []Action `json:"actions"`
}

type entry struct {
	name string
	info os.FileInfo
}

type plannedDirectory struct {
	rootPath string
	relative string
	rootInfo os.FileInfo
	info     os.FileInfo
}

// BuildPlan validates the config against the filesystem and produces the plan.
// It never modifies anything.
func BuildPlan(cfg *Config) (*Plan, error) {
	roots, err := resolveRoots(cfg.RootsAllowlist)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	for _, rule := range cfg.Rules {
		var acts []Action
		var err error
		switch rule.Kind {
		case kindPrune, kindPurge:
			acts, err = planPathRule(rule, roots)
		case kindExec:
			acts = []Action{{Rule: rule.Name, Op: opExec, Command: rule.Command}}
		}
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
		}
		plan.Actions = append(plan.Actions, acts...)
	}
	return plan, nil
}

func planPathRule(rule Rule, roots []string) ([]Action, error) {
	rootPath, relative, err := resolveRulePath(rule.Path, roots)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	dir, err := root.OpenRoot(relative)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat(".")
	if err != nil {
		return nil, err
	}
	if os.SameFile(info, rootInfo) {
		return nil, fmt.Errorf("path %s is an allowlisted root", rule.Path)
	}

	var actions []Action
	if rule.Kind == kindPrune {
		actions, err = planPrune(rule, dir)
	} else {
		actions, err = planPurge(rule, dir)
	}
	if err != nil {
		return nil, err
	}
	directory := &plannedDirectory{rootPath: rootPath, relative: relative, rootInfo: rootInfo, info: info}
	for i := range actions {
		if actions[i].Op == opDelete {
			actions[i].directory = directory
		}
	}
	return actions, nil
}

func planPrune(rule Rule, dir *os.Root) ([]Action, error) {
	entries, err := listEntries(dir, true)
	if err != nil {
		return nil, err
	}

	edges := bucketEdges(rule)
	keeps := keepCounts(rule)
	always := alwaysKeep(rule)
	grace := time.Duration(rule.GraceHours * float64(time.Hour))

	reason := make([]string, len(entries))
	members := map[int][]int{}

	for i, e := range entries {
		age := time.Since(e.info.ModTime())
		switch {
		case age < grace:
			reason[i] = fmt.Sprintf("younger than grace (%.0fh)", rule.GraceHours)
		case i >= len(entries)-always:
			reason[i] = "newest"
		default:
			b := bucketIndex(age.Hours()/24, edges)
			members[b] = append(members[b], i)
		}
	}

	// entries are oldest first, so the tail of each bucket is its newest part.
	for b, idxs := range members {
		keep := keeps[b]
		start := len(idxs) - keep
		if start < 0 {
			start = 0
		}
		for j := start; j < len(idxs); j++ {
			rank := len(idxs) - j
			label := bucketLabel(b, edges)
			if rank == 1 {
				reason[idxs[j]] = fmt.Sprintf("newest in bucket %s", label)
			} else {
				reason[idxs[j]] = fmt.Sprintf("rank %d in bucket %s", rank, label)
			}
		}
	}

	acts := make([]Action, 0, len(entries))
	for i, e := range entries {
		path := filepath.Join(rule.Path, e.name)
		if reason[i] != "" {
			acts = append(acts, Action{Rule: rule.Name, Op: opKeep, Path: path, Reason: reason[i]})
			continue
		}
		if protected(rule.Protected, e.name) {
			acts = append(acts, Action{Rule: rule.Name, Op: opKeep, Path: path, Reason: "protected by pattern"})
			continue
		}
		size, err := pathSize(dir, e.name)
		if err != nil {
			return nil, err
		}
		acts = append(acts, Action{Rule: rule.Name, Op: opDelete, Path: path, Reason: "outside retention", Bytes: size, fileInfo: e.info})
	}
	return acts, nil
}

func planPurge(rule Rule, dir *os.Root) ([]Action, error) {
	entries, err := listEntries(dir, false)
	if err != nil {
		return nil, err
	}

	grace := time.Duration(rule.GraceHours * float64(time.Hour))
	acts := make([]Action, 0, len(entries))
	for _, e := range entries {
		path := filepath.Join(rule.Path, e.name)
		if time.Since(e.info.ModTime()) < grace {
			acts = append(acts, Action{Rule: rule.Name, Op: opKeep, Path: path,
				Reason: fmt.Sprintf("younger than grace (%.0fh)", rule.GraceHours)})
			continue
		}
		if protected(rule.Protected, e.name) {
			acts = append(acts, Action{Rule: rule.Name, Op: opKeep, Path: path, Reason: "protected by pattern"})
			continue
		}
		size, err := pathSize(dir, e.name)
		if err != nil {
			return nil, err
		}
		acts = append(acts, Action{Rule: rule.Name, Op: opDelete, Path: path, Bytes: size, fileInfo: e.info})
	}
	return acts, nil
}

func listEntries(dir *os.Root, directoriesOnly bool) ([]entry, error) {
	des, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return nil, err
	}

	out := make([]entry, 0, len(des))
	for _, de := range des {
		info, err := dir.Lstat(de.Name())
		if err != nil {
			continue
		}
		// Symlinks are skipped rather than followed: following one would let a
		// link inside the tree point the walk outside the allowlist.
		if info.Mode()&os.ModeSymlink != 0 || (directoriesOnly && !info.IsDir()) {
			continue
		}
		out = append(out, entry{name: de.Name(), info: info})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].info.ModTime().Before(out[j].info.ModTime()) })
	return out, nil
}

func resolveRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("roots_allowlist entry %q: %w", root, err)
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

// resolveRulePath rejects paths outside the allowlist, including the allowlist
// roots themselves. The check runs on the symlink-resolved path, so a symlinked
// rule path cannot escape the sandbox.
func resolveRulePath(path string, roots []string) (string, string, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return "", "", err
	}
	var rootPath, relative string
	for _, root := range roots {
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			continue
		}
		if rel == "." {
			return "", "", fmt.Errorf("path %s is an allowlisted root", path)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(rootPath) {
			rootPath, relative = root, rel
		}
	}
	if rootPath == "" {
		return "", "", fmt.Errorf("path %s resolves to %s, outside roots_allowlist", path, resolved)
	}
	return rootPath, relative, nil
}

func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Abs(resolved)
	}
	parentPath := filepath.Dir(path)
	if !os.IsNotExist(err) || parentPath == path {
		return "", err
	}
	parent, err := resolvePath(parentPath)
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(parent, filepath.Base(path)))
}
