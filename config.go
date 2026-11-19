package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Rule kinds.
const (
	kindPrune = "prune"
	kindPurge = "purge"
	kindExec  = "exec"
)

// Config is the on-disk policy file.
//
// RootsAllowlist bounds every path rule: a rule whose path resolves outside one
// of these roots is rejected before any planning happens. ExecAllowlist bounds
// every binary an exec rule may run.
type Config struct {
	Version        int      `json:"version"`
	RootsAllowlist []string `json:"roots_allowlist"`
	ExecAllowlist  []string `json:"exec_allowlist"`
	Rules          []Rule   `json:"rules"`
}

// Rule is one cleanup instruction.
//
// prune thins a directory of timestamped snapshots by age.
// purge empties a cache directory.
// exec runs an external cleaner, for the cases where deleting files directly
// would corrupt the owning tool's own bookkeeping.
type Rule struct {
	Name string `json:"name"`
	Kind string `json:"kind"`

	// prune and purge
	Path      string   `json:"path,omitempty"`
	Protected []string `json:"protected,omitempty"` // globs matched against entry names, never deleted

	// prune
	GraceHours    float64   `json:"grace_hours,omitempty"`     // entries younger than this are always kept
	BucketDays    []float64 `json:"bucket_days,omitempty"`     // ascending age edges, default [2 4 8 16 32]
	KeepPerBucket []int     `json:"keep_per_bucket,omitempty"` // per bucket, default 1 each; bucket 0 is the youngest
	AlwaysKeep    int       `json:"always_keep,omitempty"`     // newest N entries kept regardless, default 2

	// purge
	// (GraceHours and Protected apply here too.)

	// exec
	Command []string `json:"command,omitempty"`
}

// LoadConfig reads and validates a policy file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(raw))), cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Rules) == 0 {
		return fmt.Errorf("no rules defined")
	}
	if len(c.RootsAllowlist) == 0 {
		return fmt.Errorf("roots_allowlist is required")
	}
	for _, root := range c.RootsAllowlist {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("roots_allowlist entry %q must be absolute", root)
		}
	}

	for i, r := range c.Rules {
		if r.Name == "" {
			return fmt.Errorf("rule %d has no name", i)
		}
		if len(r.Protected) != 0 && r.Kind == kindExec {
			return fmt.Errorf("rule %q: protected does not apply to exec rules", r.Name)
		}

		switch r.Kind {
		case kindPrune, kindPurge:
			if r.Path == "" {
				return fmt.Errorf("rule %q: path is required", r.Name)
			}
			if !filepath.IsAbs(r.Path) {
				return fmt.Errorf("rule %q: path must be absolute", r.Name)
			}
			if r.GraceHours < 0 {
				return fmt.Errorf("rule %q: grace_hours must not be negative", r.Name)
			}
			if r.GraceHours*float64(time.Hour) >= float64(math.MaxInt64) {
				return fmt.Errorf("rule %q: grace_hours exceeds the maximum duration", r.Name)
			}
			for _, pattern := range r.Protected {
				if _, err := path.Match(pattern, ""); err != nil {
					return fmt.Errorf("rule %q: invalid protected pattern %q: %w", r.Name, pattern, err)
				}
			}
			if r.Kind == kindPrune {
				if err := validateBuckets(r); err != nil {
					return err
				}
			}
		case kindExec:
			if len(r.Command) == 0 {
				return fmt.Errorf("rule %q: command is required", r.Name)
			}
			if !c.execAllowed(r.Command[0]) {
				return fmt.Errorf("rule %q: %q is not in exec_allowlist", r.Name, r.Command[0])
			}
		default:
			return fmt.Errorf("rule %q: unknown kind %q", r.Name, r.Kind)
		}
	}
	return nil
}

func validateBuckets(r Rule) error {
	if r.AlwaysKeep < 0 {
		return fmt.Errorf("rule %q: always_keep must not be negative", r.Name)
	}
	for i := 1; i < len(r.BucketDays); i++ {
		if r.BucketDays[i] <= r.BucketDays[i-1] {
			return fmt.Errorf("rule %q: bucket_days must be strictly ascending", r.Name)
		}
	}
	bucketCount := len(bucketEdges(r)) + 1
	if len(r.KeepPerBucket) != 0 && len(r.KeepPerBucket) != bucketCount {
		return fmt.Errorf("rule %q: keep_per_bucket needs %d entries, got %d", r.Name, bucketCount, len(r.KeepPerBucket))
	}
	for _, k := range r.KeepPerBucket {
		if k < 0 {
			return fmt.Errorf("rule %q: keep_per_bucket values must not be negative", r.Name)
		}
	}
	return nil
}

func (c *Config) execAllowed(bin string) bool {
	base := filepath.Base(bin)
	for _, allowed := range c.ExecAllowlist {
		if filepath.Base(allowed) == base {
			return true
		}
	}
	return false
}

// bucketEdges returns the configured edges or the default geometric scale.
func bucketEdges(r Rule) []float64 {
	if len(r.BucketDays) != 0 {
		return r.BucketDays
	}
	return []float64{2, 4, 8, 16, 32}
}

// keepCounts returns how many entries each bucket keeps.
func keepCounts(r Rule) []int {
	edges := bucketEdges(r)
	if len(r.KeepPerBucket) != 0 {
		return r.KeepPerBucket
	}
	counts := make([]int, len(edges)+1)
	for i := range counts {
		counts[i] = 1
	}
	return counts
}

func alwaysKeep(r Rule) int {
	if r.AlwaysKeep != 0 {
		return r.AlwaysKeep
	}
	return 2
}

// bucketIndex maps an age in days onto a bucket. Bucket i spans
// [edges[i-1], edges[i]) with bucket 0 starting at zero and the final bucket
// swallowing everything past the last edge.
func bucketIndex(ageDays float64, edges []float64) int {
	for i, edge := range edges {
		if ageDays < edge {
			return i
		}
	}
	return len(edges)
}

// bucketLabel describes a bucket for human output, e.g. "0-2d" or "32d+".
func bucketLabel(index int, edges []float64) string {
	if index == 0 {
		return fmt.Sprintf("0-%gd", edges[0])
	}
	if index >= len(edges) {
		return fmt.Sprintf("%gd+", edges[len(edges)-1])
	}
	return fmt.Sprintf("%g-%gd", edges[index-1], edges[index])
}

func protected(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}
