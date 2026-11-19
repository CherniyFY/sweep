# sweep

Policy-driven cleanup for the junk that development tools leave behind, with a
dry run first and a byte count for every rule.

Two problems it targets:

- **Caches regrow silently.** A Go build cache, a container build cache and a
  shader cache can add tens of gigabytes in a day and still look like "nothing
  changed" in your home directory.
- **Backup directories only grow.** Tools that snapshot a project per session
  never prune themselves, so recovery points pile up until the disk is full.

`sweep` reads a policy file, previews the cleanup, and only acts with `--apply`.
Use `--json` to see every planned action.

## Install

With Go 1.25 or later:

```
go install github.com/CherniyFY/sweep@latest
```

Or build from a checkout:

```
go build -o sweep .
install -m 0755 sweep ~/.local/bin/
```

## Use

```
sweep                      # dry run, prints the plan
sweep -v                   # also show what is kept, and why
sweep --apply              # execute it
sweep --json               # machine-readable plan
sweep --config my.json
```

Config lookup order: `--config`, `$SWEEP_CONFIG`, `./sweep.json`,
`$XDG_CONFIG_HOME/sweep/config.json`.

Exit code is `0` on success, `1` when configuration is invalid or an action
failed. A failing delete does not abort the remaining actions; failures are
reported and summarised at the end.

## Rules

Three kinds, all bounded by the allowlists.

### `prune` — thin a directory of timestamped snapshots

Keeps history dense near now and sparse going back.

```json
{
  "name": "session snapshots",
  "kind": "prune",
  "path": "${HOME}/sweep-backups",
  "grace_hours": 24,
  "bucket_days": [2, 4, 8, 16, 32],
  "keep_per_bucket": [2, 1, 1, 1, 1, 1],
  "always_keep": 2,
  "protected": ["*.lock", "*-preflight*"]
}
```

- Entries are top-level subdirectories, ordered by modification time.
- `grace_hours`: anything younger is never deleted. Protects work in progress.
- `always_keep`: the newest N entries are kept regardless of bucket. Default 2.
- `bucket_days`: ascending age edges, default `[2, 4, 8, 16, 32]`. With those
  edges the buckets are `0-2d`, `2-4d`, `4-8d`, `8-16d`, `16-32d`, `32d+`.
- `keep_per_bucket`: how many to keep per bucket, youngest bucket first. Supply
  one count per effective bucket (six with the default edges). The default is
  one per bucket.
- `protected`: glob patterns matched against entry names. A protected entry is
  never deleted, whatever the policy says.

Symlinks and plain files are skipped entirely, so a link inside the tree cannot
redirect a delete outside it.

### `purge` — empty a cache directory

```json
{ "name": "go build cache", "kind": "purge", "path": "${HOME}/.cache/go-build" }
```

Deletes the directory's entries, not the directory itself, so the owning tool
keeps working. `grace_hours` and `protected` apply here too.

### `exec` — hand off to the tool that owns the data

Some caches cannot be deleted behind their owner's back: removing Docker's
`/var/lib/docker` contents directly corrupts its metadata, and volumes can hold
irreplaceable data. For those cases, run the tool's own pruner.

```json
{ "name": "docker images", "kind": "exec", "command": ["docker", "image", "prune", "-a", "-f"] }
```

`exec` rules require the executable's base name in `exec_allowlist`. Sweep
passes the argument array directly to the configured program. Use policy files
you trust.

Note that `sweep` cannot know how many bytes an `exec` rule reclaims; it reports
the tool's own output instead.

## Safety model

- **Dry run is the default.** `--apply` is opt-in on every invocation.
- **`roots_allowlist`.** Rule paths are symlink-resolved and must land strictly
  inside an allowed root. The root itself is rejected, so a rule cannot empty
  your home directory.
- **Scoped deletion.** Sweep checks that the planned directories and entries
  still match before deleting, and confines deletion to each rule's directory.
- **`exec_allowlist`.** Each external command needs an allowlisted executable
  name.
- **Grace windows.** Both delete kinds skip anything modified recently.
- **No shell interpolation.** Paths are used as given after env expansion;
  commands are argument arrays.
- **Measured, not guessed.** Every delete action carries its apparent size,
  computed before anything is removed.

## Example for a full dev box

Copy [the example policy](example/sweep.json) and adjust the paths and Docker
rules for your machine. Preview the plan before using `--apply`.

A typical session-end run:

```
$ sweep -v
go build cache
  delete  4.2 GiB   /home/you/.cache/go-build/00
  ...

session snapshots
  delete  387 MiB   /home/you/sweep-backups/2026-11-18-ef03
  keep              /home/you/sweep-backups/2026-11-18-ef06   newest

docker images
  exec              docker image prune -a -f

31 to delete (9.1 GiB), 2 to run, 14 kept
```

## Limitations

- Apparent size, not disk blocks: tiny-file caches read smaller than `du` says.
- `prune` orders by mtime only. Naming conventions are ignored, so a snapshot
  with a copied mtime sorts where its timestamp says, not where its name says.
- No locking. Do not run it while a session is writing into a pruned tree; the
  grace window is the intended mitigation.
- Hardlinks and reflinks can make the reclaimed space smaller than reported.
- Linux and macOS only; nothing here is Windows-aware.

## Tests

```
go test ./...
```

Tests cover retention buckets, grace windows, protected patterns, size
estimates, config validation and filesystem changes between plan and apply.
