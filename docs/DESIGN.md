# BFBackup design

BFBackup copies files from one Linux (Unraid) server to another and stores them
as plain files. One **controller** owns all configuration, the web UI and the
database. Any number of **peers** connect to it and only do what the controller
asks them to.

## Components

```
┌──────────────── controller container ────────────────┐        ┌──── peer container ────┐
│ web UI + JSON API (:7700)                            │        │ agent                  │
│ scheduler · run manager · notifier (Pushover, email) │  TCP   │  dials controller      │
│ engine (backup / verify / restore / delete)          │◀──────▶│  RPC server            │
│ SQLite (config, index, history)                      │ :7701  │  local endpoint (fsx)  │
│ local endpoint (fsx)                                  │        │  hooks                 │
└──────────────────────────────────────────────────────┘        └────────────────────────┘
```

* `cmd/bfbackup` — one binary, `controller` or `peer` role.
* `internal/model` — shared types (entries, listings, error codes).
* `internal/match` — exclude patterns (gitignore-like, `**` supported).
* `internal/fsx` — all file system access. Every path goes through an
  `os.Root`, so nothing outside the configured roots (`BFB_ROOTS`) can be read
  or written, including via symlinks or `..`.
* `internal/hooks` — runs pre/post scripts by name from `/config/hooks`.
* `internal/endpoint` — the `Endpoint` interface. `Local` wraps fsx + hooks.
  `rpc.Remote` implements the same interface over the network. The engine only
  sees `Endpoint`, so push, pull, local→local and peer→peer (relayed) are the
  same code path.
* `internal/proto` — framing (length-prefixed JSON + chunked binary) and the
  authentication handshake. `Security` wraps the raw connection: `Plain`,
  `TLS`, or `Auto` (controller; tells TLS and plain apart by the first byte).
* `internal/rpc` — request/response over yamux streams; `Server` (peer side)
  and `Remote` (controller side).
* `internal/peer` — peer agent: dial, authenticate, serve, reconnect.
* `internal/store` — SQLite (pure Go, no cgo), migrations, all queries.
* `internal/engine` — backup planning/diff, transfer, verify, restore,
  delete, index rebuild. No knowledge of HTTP or scheduling.
* `internal/notify` — `Notifier` interface; Pushover and email (SMTPS)
  implementations, `Multi` sends to every configured channel.
* `internal/logbook` — activity log for the UI in its own SQLite file
  (`logs.db`, not part of the DB snapshots). Levels: 1 errors, 2 events
  (runs, peers, settings, maintenance), 3 every file operation. Entries are
  written in batches by one goroutine; the engine reports through the
  `engine.Activity` interface, so it does not know about the log.
* `internal/controller` — node registry, run manager, scheduler, maintenance,
  self-backup.
* `internal/web` — HTTP API, auth, static UI (vanilla JS, embedded, no build
  step, no npm supply chain).

## Connection and authentication

* The peer dials the controller (`BFB_CONTROLLER=host:7701`). The controller
  needs no access to the peer's network.
* Adding a peer in the UI creates a token `bfb1.<node-id>.<256-bit secret>`.
  The peer only needs `BFB_CONTROLLER` and `BFB_TOKEN`.
* The secret never crosses the wire. Handshake: both sides send random nonces
  and prove knowledge of the secret with HMAC-SHA256 over
  `role | node-id | nonce_peer | nonce_controller`. The **peer verifies the
  controller first**, so a fake controller cannot read the peer's files.
  Replays fail because of fresh nonces.
* After the handshake the TCP connection carries a yamux session. The
  controller sends keepalives every 30 s; a dead session marks the node
  offline, the peer reconnects with exponential backoff.
* Transport: TLS 1.3 with a self-signed, in-memory controller certificate
  that nobody verifies. Instead, protocol version 2 adds 32 bytes exported
  from the TLS session (`EXPORTER-bfbackup-auth`) to both HMAC proofs. A man
  in the middle has a different TLS session with each side, so the
  controller's proof he relays does not match what the peer computes, and
  the peer stops before sending its own proof. On plain connections the
  binding is empty.
* Migration: the controller accepts version 1 (no binding field) only on
  plain connections, so old peers keep working. A version 2 peer never falls
  back to plain on its own (no downgrade); `BFB_TRANSPORT=plain` is explicit.
  `BFB_TRANSPORT=tls` on the controller answers plain hellos with a refusal.

## Slow and unstable links

Built for WAN peers at a few Mbit/s whose connection drops now and then.

* The yamux keepalive runs every 30 s and gives up after 2 min, so a full
  uplink (keepalives queue behind file data) does not count as dead.
* `Controller.Endpoint` returns an endpoint that looks up the peer's
  current session on every call, so a run goes on after a reconnect.
  Transport errors are `model.ErrConnLost` (a stall is `rpc.ErrStalled`, also
  an `ErrConnLost`), errors of the peer keep their codes.
* Simple calls that are safe to repeat (stat, list, hash, statfs, purge,
  symlink) wait up to 2 min for the peer and run once more. Trash, remove
  and hooks are never repeated blindly.
* A file whose transfer breaks waits up to 10 min for the peer and is tried
  again (3 attempts). A scan cut off by a drop starts over (3 attempts). A
  peer that stays away longer stops the run with one error; copied files are
  in the index, the next run continues with the rest.
* Resume: files of 64 MB or more are written to
  `.BFBackup-r<path key>-<size/mtime key>.tmp`, which an aborted transfer keeps.
  The next attempt asks the target how much is there (`write` with
  `resume`), the source reads and hashes that part locally (`readat`) and
  sends only the rest, so the SHA-256 still covers the whole file. A
  changed source file gets a new key and starts over; a failed commit drops
  the partial file; old partials in a directory are removed after 7 days.
* Compression: zstd (fastest level) for file data and scan listings, asked
  for per call and confirmed by the other side, so old peers get plain data.
  Already compressed types (video, images, archives, …) are sent as they
  are. `BFB_COMPRESS=off` on the controller turns it off.

## Data model on disk

* Target layout = source layout below the job's source path. Plain files,
  mode bits and mtime preserved. Ownership is not preserved; optionally set via
  `BFB_UID` / `BFB_GID` (Unraid: 99/100).
* Files are written to `.BFBackup-<random>.tmp` in the target directory,
  hashed while writing, compared with the sender's hash, then renamed into
  place. A crash never leaves a half-written file under the real name. The
  `.bfb-*.tmp` names of older versions are still internal (never scanned or
  copied) and old partials are cleaned up.
* `.bfbackup-trash/<batch>/<path>` below a job root holds what BFBackup
  removes (deletes, sync removals, excluded files) and what a restore
  overwrites. Backups overwrite changed files in place (temp file renamed over
  the old copy), so the trash does not double the target's size. Purged after
  the configured retention, by the batch's time.
* A batch is named `<UTC start>-run<id>` (e.g. `20261025T101500Z-run1234`):
  one batch per run, whatever node it is on. The controller sends the name
  with every `trash` call; older controllers send none and the peer names the
  batch after the current second, as before. A second copy of a path in one
  batch gets a `-<n>` suffix. `trashinfo` walks a root's trash and returns
  each batch's file count and size; the UI asks for it only when the trash
  panel is shown (Settings, job page). Older peers answer "unsupported".
* Versioning is not implemented. The place to add it is the transfer step:
  before overwriting, move the old file to a version store (same mechanism as
  the trash).

## Index

`files(target_id, dir, name, …)` stores, per target, what has been copied:
size, mtime, mode, SHA-256 and the last scan that saw it on the source. The
diff runs **directory by directory** against this index, so memory stays flat
with millions of files and the target disks are not touched for planning
(important for Unraid spin-down).

Compare modes per job:

| mode       | a file is copied when…                         | cost                    |
|------------|------------------------------------------------|-------------------------|
| `size`     | size or type changed                           | metadata only           |
| `mtime`    | size, mtime or type changed (default)          | metadata only           |
| `checksum` | size, type or SHA-256 changed                  | reads every source file |

Every transfer is hashed end to end, regardless of mode.

## Runs

A run is one operation on one target: `backup`, `dryrun`, `verify_quick`,
`verify_deep`, `restore`, `delete`, `rebuild_index`.

Queue (`controller/queue.go`): a run starts when its targets and the peers
it uses (source, target, restore destination; not the controller itself)
have no other run and, for backup/verify/rebuild, one of the parallel slots
is free. First in, first out; a waiting run reserves its peers and targets,
so later runs cannot overtake it, and it takes everything at once, so runs
never deadlock. A second run of the same kind on a target is refused
(`ErrBusy`); the scheduler logs it as skipped.

Backup flow:

1. Pre-hook on the source node (failure aborts the run).
2. Scan the source (parallel directory walk, excludes applied on the source).
3. Diff against the index → new / changed / unchanged; write the plan to a temp
   file. Files missing on the source are only counted ("not in source");
   they are **never deleted on the target automatically**.
4. Safety checks:
   * source scan empty but index not → fail (unmounted disk).
   * changed files above `protect_pct` % and `protect_min` → run stops in
     `awaiting_approval` and notifies (ransomware / mass rewrite guard).
   * bytes to copy > free space on the target → fail.
5. Dry run stops here and stores a sample of the plan.
6. Parallel transfer (`workers` per job), index updated in batches.
7. Post-hook (gets `BFB_RUN_STATUS`), notifications, statistics.

Verify: `quick` scans the target and compares size/mtime with the index;
`deep` re-hashes every target file and compares SHA-256. Optional repair drops
broken entries from the index so the next backup copies them again.

Restore: from the index, for any set of files/folders, to the original location
or any node/path; only missing or different files are copied unless overwrite
is requested. Files that get overwritten go to the trash first.

Delete: selected paths are moved to the trash on the source and on every
target of the job (or on the target only, e.g. for "not in source" files) and
removed from the index.

## USB drives (offline targets)

USB drives plugged into the controller (not into peers) are backup targets
that are usually not there. They have their own page; jobs use them as
targets.

* **A drive is a virtual node `usb:<uuid>`.** Its targets are ordinary
  targets with that `node_id` and a path relative to the drive's root
  (default `/<job name>`). `Controller.Endpoint` returns a `driveEP` that
  maps those paths to the current mount point and passes them to the local
  endpoint. So engine, index, restore, verify, browse, trash and the queue
  work unchanged; the queue treats the drive like a peer (one run on it at a
  time), so its targets run one after another.
* **Identity:** "Set up" writes `/.bfbackup-drive.json` (temp file + rename):
  `uuid`, `name`, `controller` (the `controller_id` setting, created once
  and part of the config export), `created`. A drive is recognized only by
  this marker, never by label or mount path. A marker of another controller
  is shown as "belongs to another BFBackup" and never written to; a marker of
  this controller whose drive was forgotten can be set up again with the same
  ID (files already there are taken over). Two mounts with the same ID (a
  cloned drive) are refused. Nothing is formatted or encrypted: the container
  is not privileged; that stays in Unassigned Devices (LUKS recommended for
  drives that leave the house).
* **Detection:** every 20 s (and on every load of the drives page) the
  controller reads the mounts below the roots (`fsx.Disks`) and their
  markers. Candidates leave out the roots themselves, `/mnt/user`,
  `/mnt/user0`, network file systems and `remotes/`. A drive that appears
  starts its backups when "back up when plugged in" is on; on the first scan
  after a start only if it was not seen for 10 min (a restart is no plug
  in). A drive that disappears is logged.
* **Mount guard:** `driveEP` checks before every call, and before a file is
  moved into place, that the mount point is still one (`fsx.IsMountPoint`:
  other device than its parent), and `Endpoint` also checks the marker. An
  unplugged drive leaves an empty folder `/mnt/disks/<label>`; writing there
  would fill Unraid's root file system, which is RAM. A failed check and
  EIO/ENODEV/ENXIO/ESTALE wrap `model.ErrOffline`, which stops a run at once
  (copied files stay in the index, partials resume next time).
* **Store:** `drives(uuid, name, created_at, last_seen, last_mount, fstype,
  total, free, on_connect, verify_after, overdue_days, overdue_notified)`
  (migration 7); forgetting a drive deletes its targets and their index,
  never files. Drives are part of the config export.
* **Runs:** "back up when plugged in" and "Back up all now" queue a backup
  of every enabled target on the drive whose source is online, with a quick
  verify after each if `verify_after`. When all runs of that batch are done,
  a `drive_done` notification sums them up and says whether the drive is safe
  to unmount. Drive targets have no schedule (refused by the API). A drive
  with targets that was not seen for `overdue_days` sends `drive_overdue`,
  repeated every `overdue_days`.
* **File systems:** xfs, ext4, btrfs are fine; exfat, ntfs and vfat get a
  warning in the setup dialog and on the drive card.
* **UI:** nav entry "USB drives" (`#/drives`): new drives with "Set up…"
  (name, jobs to add as targets, back up now, back up when plugged in,
  verify after, overdue days) and one card per drive (connected or last
  seen, in use or safe to unmount, overdue, capacity, its jobs with size,
  last backup and verify, "Back up all now", "Verify all", "Add jobs…",
  history filtered to the drive, settings, forget). Job targets can pick a
  USB drive instead of a node; the dashboard has a USB tile.

## Security summary

* Web UI: single admin user, bcrypt password, random session tokens (only the
  SHA-256 is stored), `HttpOnly` + `SameSite=Strict` cookie, custom header
  required on every state-changing request (CSRF), login rate limit, strict
  CSP, body size limits.
* Peers: HMAC challenge-response, controller authenticated first, frame size
  limits, I/O deadlines, path confinement via `os.Root`, hooks only by name
  from the peer's own hooks folder (the controller cannot send commands).
* Destructive actions go to the trash and are written to the audit log.

## Testing

* Unit tests per package with temp dirs and a real SQLite file (pure Go).
* `rpc` tests run `Server` and `Remote` over an in-memory pipe and check that
  `Remote` behaves exactly like `Local`.
* `engine` tests cover backup, incremental runs, protection, dry run, verify,
  restore, delete and rebuild with local endpoints.
* `controller` integration test starts a controller and a peer over loopback
  TCP and runs push and pull backups end to end.
* `web` tests use `httptest` for auth, CSRF and the main API flows.

## Extension points

* New transport security: implement `proto.Security`.
* New notification channel: implement `notify.Notifier`.
* Versioning: transfer step in `engine` + a version store next to the trash.
* New run kinds: add to `engine`, register in `controller.Runs`.
