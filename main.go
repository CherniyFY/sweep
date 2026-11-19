package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
}

func run() error {
	configFlag := flag.String("config", "", "policy file (default: $SWEEP_CONFIG, ./sweep.json, then $XDG_CONFIG_HOME/sweep/config.json)")
	apply := flag.Bool("apply", false, "execute the plan")
	asJSON := flag.Bool("json", false, "print the plan as JSON and exit")
	showKept := flag.Bool("v", false, "also list the entries that are kept")
	flag.Parse()

	path, err := findConfig(*configFlag)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}

	plan, err := BuildPlan(cfg)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}

	printPlan(plan, *showKept)

	if !*apply {
		fmt.Printf("\nconfig: %s\ndry run. re-run with --apply to execute.\n", path)
		return nil
	}

	if err := applyPlan(plan); err != nil {
		return err
	}
	return nil
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}

	candidates := []string{os.Getenv("SWEEP_CONFIG"), "sweep.json"}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "sweep", "config.json"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no policy file found; pass --config or set SWEEP_CONFIG")
}

// printPlan renders the plan grouped by rule. Kept entries are hidden unless
// verbose, so the default output shows only what would change. Long delete
// lists are collapsed into one summary line so purging a cache with hundreds of
// shards stays readable.
func printPlan(plan *Plan, showKept bool) {
	var order []string
	groups := map[string][]Action{}
	for _, a := range plan.Actions {
		if _, seen := groups[a.Rule]; !seen {
			order = append(order, a.Rule)
		}
		groups[a.Rule] = append(groups[a.Rule], a)
	}

	var deleteCount int
	var deleteBytes int64
	var execCount int
	var keptCount int

	for _, rule := range order {
		fmt.Printf("\n%s\n", rule)

		var deletes []Action
		for _, a := range groups[rule] {
			switch a.Op {
			case opKeep:
				keptCount++
				if showKept {
					fmt.Printf("  keep    %-9s %s  (%s)\n", "", a.Path, a.Reason)
				}
			case opDelete:
				deletes = append(deletes, a)
			case opExec:
				execCount++
				fmt.Printf("  exec    %-9s %s\n", "", joinCommand(a.Command))
			}
		}

		if len(deletes) == 0 {
			continue
		}
		var bytes int64
		for _, a := range deletes {
			bytes += a.Bytes
		}
		deleteCount += len(deletes)
		deleteBytes += bytes

		if len(deletes) == 1 {
			fmt.Printf("  delete  %-9s %s\n", formatBytes(bytes), deletes[0].Path)
			continue
		}
		fmt.Printf("  delete  %-9s %s/%s  (%d entries)\n",
			formatBytes(bytes), filepath.Dir(deletes[0].Path), "*", len(deletes))
	}

	fmt.Printf("\n%d to delete (%s), %d to run, %d kept\n", deleteCount, formatBytes(deleteBytes), execCount, keptCount)
}

func joinCommand(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// applyPlan executes delete and exec actions. A failing delete is reported and
// the run continues, so one unwritable path cannot abort the rest.
func applyPlan(plan *Plan) error {
	var failures int

	for _, a := range plan.Actions {
		switch a.Op {
		case opDelete:
			if err := removePlannedEntry(a); err != nil {
				fmt.Fprintf(os.Stderr, "  failed  %s: %v\n", a.Path, err)
				failures++
				continue
			}
			fmt.Printf("  removed %-9s %s\n", formatBytes(a.Bytes), a.Path)
		case opExec:
			fmt.Printf("\n$ %s\n", joinCommand(a.Command))
			cmd := exec.Command(a.Command[0], a.Command[1:]...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "  failed  %s: %v\n", joinCommand(a.Command), err)
				failures++
			}
		}
	}

	if failures != 0 {
		return fmt.Errorf("%d action(s) failed", failures)
	}
	return nil
}

func removePlannedEntry(action Action) error {
	planned := action.directory
	currentPath, err := resolvePath(filepath.Dir(action.Path))
	if err != nil {
		return err
	}
	if currentPath != filepath.Join(planned.rootPath, planned.relative) {
		return fmt.Errorf("rule path resolved to a different location after planning")
	}

	root, err := os.OpenRoot(planned.rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, planned.rootInfo) {
		return fmt.Errorf("allowlisted root changed after planning")
	}

	dir, err := root.OpenRoot(planned.relative)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, planned.info) {
		return fmt.Errorf("rule directory changed after planning")
	}

	name := filepath.Base(action.Path)
	current, err := dir.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(current, action.fileInfo) || !current.ModTime().Equal(action.fileInfo.ModTime()) {
		return fmt.Errorf("entry changed after planning")
	}
	return dir.RemoveAll(name)
}
