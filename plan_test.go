package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestBucketIndex(t *testing.T) {
	edges := []float64{2, 4, 8, 16, 32}
	cases := []struct {
		ageDays float64
		want    int
	}{
		{0, 0}, {1.9, 0}, {2, 1}, {3.9, 1}, {4, 2},
		{7.9, 2}, {8, 3}, {15, 3}, {16, 4}, {31, 4},
		{32, 5}, {400, 5},
	}
	for _, c := range cases {
		if got := bucketIndex(c.ageDays, edges); got != c.want {
			t.Errorf("bucketIndex(%v) = %d, want %d", c.ageDays, got, c.want)
		}
	}
}

func TestBucketLabel(t *testing.T) {
	edges := []float64{2, 4, 8, 16, 32}
	want := map[int]string{0: "0-2d", 1: "2-4d", 4: "16-32d", 5: "32d+"}
	for i, w := range want {
		if got := bucketLabel(i, edges); got != w {
			t.Errorf("bucketLabel(%d) = %q, want %q", i, got, w)
		}
	}
}

func TestProtected(t *testing.T) {
	patterns := []string{"*.lock", "keep-*"}
	if !protected(patterns, "state.lock") {
		t.Error("expected state.lock to be protected")
	}
	if !protected(patterns, "keep-this") {
		t.Error("expected keep-this to be protected")
	}
	if protected(patterns, "drop-me") {
		t.Error("expected drop-me not to be protected")
	}
}

// makeTree creates root/backups/name directories and backdates them.
func makeTree(t *testing.T, ages map[string]time.Duration) string {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "backups")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, age := range ages {
		path := filepath.Join(target, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func pruneConfig(root string, graceHours float64, keeps []int) *Config {
	return &Config{
		Version:        1,
		RootsAllowlist: []string{root},
		Rules: []Rule{{
			Name:          "snapshots",
			Kind:          kindPrune,
			Path:          filepath.Join(root, "backups"),
			GraceHours:    graceHours,
			KeepPerBucket: keeps,
		}},
	}
}

func deletedPaths(t *testing.T, plan *Plan) []string {
	t.Helper()
	var out []string
	for _, a := range plan.Actions {
		if a.Op == opDelete {
			out = append(out, filepath.Base(a.Path))
		}
	}
	sort.Strings(out)
	return out
}

func TestPruneGeometric(t *testing.T) {
	day := 24 * time.Hour
	root := makeTree(t, map[string]time.Duration{
		"a-1h":  1 * time.Hour,
		"b-3h":  3 * time.Hour,
		"c-3d":  3 * day,
		"d-5d":  5 * day,
		"e-9d":  9 * day,
		"f-20d": 20 * day,
		"g-40d": 40 * day,
		"h-60d": 60 * day,
	})

	plan, err := BuildPlan(pruneConfig(root, 0, nil))
	if err != nil {
		t.Fatal(err)
	}

	// One representative per bucket plus the newest two: only 60d falls out.
	got := deletedPaths(t, plan)
	if len(got) != 1 || got[0] != "h-60d" {
		t.Fatalf("deleted = %v, want [h-60d]", got)
	}
}

func TestPruneGraceAndBucketZero(t *testing.T) {
	hour := time.Hour
	root := makeTree(t, map[string]time.Duration{
		"a-3h":  3 * hour,
		"b-6h":  6 * hour,
		"c-30h": 30 * hour,
		"d-40h": 40 * hour,
		"e-60h": 60 * hour,
		"f-70h": 70 * hour,
	})

	// Bucket 0 keeps two entries, older buckets keep one each.
	plan, err := BuildPlan(pruneConfig(root, 24, []int{2, 1, 1, 1, 1, 1}))
	if err != nil {
		t.Fatal(err)
	}

	got := deletedPaths(t, plan)
	if len(got) != 1 || got[0] != "f-70h" {
		t.Fatalf("deleted = %v, want [f-70h]", got)
	}
}

func TestPruneProtectedPattern(t *testing.T) {
	root := makeTree(t, map[string]time.Duration{
		"a-1h":     1 * time.Hour,
		"b-3h":     3 * time.Hour,
		"old-40d":  40 * 24 * time.Hour,
		"old-50d":  50 * 24 * time.Hour,
		"old-60d":  60 * 24 * time.Hour,
		"keep-61d": 61 * 24 * time.Hour,
	})

	cfg := pruneConfig(root, 0, nil)
	cfg.Rules[0].Protected = []string{"keep-*"}

	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The oldest bucket keeps only 40d; keep-61d survives by pattern.
	got := deletedPaths(t, plan)
	if len(got) != 2 || got[0] != "old-50d" || got[1] != "old-60d" {
		t.Fatalf("deleted = %v, want [old-50d old-60d]", got)
	}
}

func TestPurgeHonoursGrace(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fresh", "stale"} {
		if err := os.WriteFile(filepath.Join(cache, name), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(cache, "stale"), old, old); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Version:        1,
		RootsAllowlist: []string{root},
		Rules: []Rule{{
			Name:       "cache",
			Kind:       kindPurge,
			Path:       cache,
			GraceHours: 24,
		}},
	}

	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var deleted []string
	for _, a := range plan.Actions {
		if a.Op == opDelete {
			if a.Bytes != 10 {
				t.Errorf("bytes = %d, want 10", a.Bytes)
			}
			deleted = append(deleted, filepath.Base(a.Path))
		}
	}
	if len(deleted) != 1 || deleted[0] != "stale" {
		t.Fatalf("deleted = %v, want [stale]", deleted)
	}
}

func TestPurgeMeasuresNestedFiles(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	shard := filepath.Join(cache, "shard")
	if err := os.MkdirAll(filepath.Join(shard, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(shard, "file"), filepath.Join(shard, "nested", "file")} {
		if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := &Config{RootsAllowlist: []string{root}, Rules: []Rule{{Name: "cache", Kind: kindPurge, Path: cache}}}
	plan, err := BuildPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Op != opDelete || plan.Actions[0].Bytes != 20 {
		t.Fatalf("expected one 20-byte deletion, got %+v", plan.Actions)
	}
}

func TestResolveRulePathRejectsOutside(t *testing.T) {
	root := t.TempDir()
	roots, err := resolveRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveRulePath("/etc", roots); err == nil {
		t.Fatal("expected /etc to be rejected")
	}
	if _, _, err := resolveRulePath(root, roots); err == nil {
		t.Fatal("expected the allowlist root itself to be rejected")
	}
	if _, _, err := resolveRulePath(filepath.Join(root, "missing", "cache"), roots); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestApplyPreservesReplacedEntry(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	unchanged := filepath.Join(cache, "unchanged")
	replaced := filepath.Join(cache, "replaced")
	for _, path := range []string{unchanged, replaced} {
		if err := os.WriteFile(path, []byte("planned"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := &Config{RootsAllowlist: []string{root}, Rules: []Rule{{Name: "cache", Kind: kindPurge, Path: cache}}}
	plan, err := BuildPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replaced, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replaced, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyPlan(plan); err == nil {
		t.Fatal("expected apply to report the changed entry")
	}
	if _, err := os.Stat(unchanged); !os.IsNotExist(err) {
		t.Fatalf("unchanged entry should be deleted, got %v", err)
	}
	if data, err := os.ReadFile(replaced); err != nil || string(data) != "replacement" {
		t.Fatalf("replacement should remain unchanged, got %q (%v)", data, err)
	}
}

func TestApplyPreservesReplacedRuleDirectory(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	entryPath := filepath.Join(cache, "entry")
	createCache := func() {
		t.Helper()
		if err := os.Mkdir(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(entryPath, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	createCache()
	config := &Config{RootsAllowlist: []string{root}, Rules: []Rule{{Name: "cache", Kind: kindPurge, Path: cache}}}
	plan, err := BuildPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cache, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	createCache()
	if err := applyPlan(plan); err == nil {
		t.Fatal("expected apply to report the changed directory")
	}
	if _, err := os.Stat(entryPath); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Rules: []Rule{{Name: "x", Kind: kindPrune, Path: "/home/x"}}},
		{Version: 1, RootsAllowlist: []string{"relative"}},
		{Version: 1, RootsAllowlist: []string{"/home/x"}, Rules: []Rule{{Name: "x", Kind: "nope"}}},
		{Version: 1, RootsAllowlist: []string{"/home/x"}, Rules: []Rule{{Name: "x", Kind: kindExec, Command: []string{"docker", "system", "prune"}}}},
		{Version: 1, RootsAllowlist: []string{"/home/x"}, Rules: []Rule{{Name: "x", Kind: kindPrune, Path: "/home/x/b", KeepPerBucket: []int{1}}}},
		{Version: 1, RootsAllowlist: []string{"/home/x"}, Rules: []Rule{{Name: "x", Kind: kindPurge, Path: "/home/x/b", GraceHours: 1e10}}},
		{Version: 1, RootsAllowlist: []string{"/home/x"}, Rules: []Rule{{Name: "x", Kind: kindPurge, Path: "/home/x/b", Protected: []string{"prefix*["}}}},
	}
	for i, cfg := range bad {
		if err := cfg.validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}

	good := Config{
		Version:        1,
		RootsAllowlist: []string{"/home/x"},
		ExecAllowlist:  []string{"docker"},
		Rules: []Rule{
			{Name: "p", Kind: kindPrune, Path: "/home/x/b", KeepPerBucket: []int{2, 1, 1, 1, 1, 1}},
			{Name: "u", Kind: kindPurge, Path: "/home/x/c", Protected: []string{"*.lock", "literal\\*name", "[*]"}},
			{Name: "e", Kind: kindExec, Command: []string{"docker", "image", "prune", "-a", "-f"}},
		},
	}
	if err := good.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
