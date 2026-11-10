# Changelog

One entry per version, newest first. "Peers" says whether the peers must be
updated too, or only the controller.

## 1.16.1 (2026-11-10)

- Settings → Retention and limits: the database snapshot can be taken
  daily, weekly or monthly (default daily), next to the number of snapshots
  to keep. A snapshot missed while the controller was down is taken at the
  next maintenance; "Run maintenance now" always takes one.
- Safety: a folder in BFB_ROOTS that is not mapped from the host is no
  longer used as is (writes would land inside the container and be lost on
  update). BFBackup uses the mapped folders below it instead and logs a
  note; if there are none, it does not start.
- Unraid templates: clearer texts for Storage and Allowed roots; Allowed
  roots is shown by default. Log level and Initial admin password are no
  longer in the templates (they still work as variables).

Peers: yes (recommended, the safety check runs on peers too).

## 1.16.0 (2026-11-07)

First public release (GitHub, Docker Hub, Unraid Community Applications).

- Settings → Display: appearance Auto / Light / Dark, and the date and time
  format for the whole UI (2026-11-07, 07.11.2026, 07/11/2026 or US
  11/07/2026 with AM/PM).
- Settings → Additional features: USB drive backups and hooks can be
  switched off; they are then hidden and do nothing. Features already in use
  stay on after the update; switching one off while it is in use asks first.
- Images and binaries for 32-bit ARM (linux/arm/v7, e.g. older Raspberry
  Pi) besides amd64 and arm64.
- Fixed: the jobs page jumped to the top every few seconds while a job ran.
- Fixed: a USB drive could show up by its ID instead of its name on the
  dashboard.
- USB drives page on phones: the jobs of a drive show as cards instead of a
  table that scrolls sideways.
- "Created by DelSol" in the sidebar.

Peers: no (the only peer-side change makes 32-bit ARM builds possible).

## 1.15.0 (2026-11-04)

- New page "USB drives": offline backups on USB drives plugged into the
  controller's server (mounted by Unassigned Devices). A new drive is set up
  once (name, which jobs go on it, back up when plugged in, quick verify
  after, reminder after N days away) and recognized by a marker file, not by
  label or mount path. Per drive: connected or last seen, in use or safe to
  unmount, overdue, capacity, and per job size, last backup and verify;
  "Back up all now", "Verify all", "Add jobs…", history, settings, forget.
- Job targets can be a USB drive (folder on the drive, no schedule).
- Mount guard: nothing is read from or written to a drive that is no longer
  mounted (the empty folder it leaves behind is RAM on Unraid); a removed
  drive stops the run at once.
- Notifications "USB drive finished (safe to unmount)" and "USB drive
  overdue", on by default. Dashboard USB tile, history filter by drive,
  drives in the config export.
- "Sync now" dialog: no more typing the job name; the red button says
  "Sync and remove extra files" or "Sync and delete permanently".

Peers: no.

## 1.14.1 (2026-11-02)

- Faster removal of files from a target (sync, removed exclusions): each
  removed file no longer reads the target's whole index, the index is
  updated 500 files at a time, and the Unraid disk list is read once every
  few seconds instead of per file.
- Target ⋯ menu: "Sync now" and "Rebuild index" without the "…".

Peers: yes (faster trash on targets held by a peer; older peers still work).

## 1.14.0 (2026-11-02)

- Targets of copy jobs have "Sync now…" in their ⋯ menu: one backup that
  also removes from that target everything no longer in the source, to the
  trash (default) or permanently. Confirmed by typing the job name; the job
  stays a copy job. The anomaly checks still stop and ask. The run shows as
  "Sync" in the history, and its approval and Try again keep the sync.

Peers: no.

## 1.13.1 (2026-11-01)

- Fix: with the single `/mnt` path, the disk statistics listed `/mnt`
  itself (the host's small RAM root file system) next to `/mnt/user`. Only
  `/mnt/user` and unassigned disks are shown again, as before 1.13.0.

Peers: yes.

## 1.13.0 (2026-11-01)

- Fix: deleting into the trash failed with "no space left on device" when
  the Unraid disk holding the file was full (or below the share's minimum
  free space), although the share had room. Files now go to the trash on
  the disk they are on, without copying; where that is not possible they
  are copied to the trash and then removed.
- Unraid templates: one path *Storage* (`/mnt` → `/mnt`, read/write slave)
  replaces *User shares* and *Unassigned disks*. Existing containers: remove
  those two paths and add `/mnt`; job paths stay the same. Without it the
  trash still works, but full disks need the copy.
- The single array disks and pools are never shown: not in the disk
  statistics (no double counting) and not in the folder picker.

Peers: yes.

## 1.12.0 (2026-11-01)

- Needs your attention: failed and incomplete runs have a "Try again" button.
  It starts the run again with the same settings; a delete is only repeated
  where it did not finish (offline or failed targets, the source only if
  deleting there failed). The old card goes away.
- Deleting on several targets no longer stops when a target's node is
  offline: it deletes on the reachable ones and ends with a warning naming
  the skipped targets. A target that goes offline during the delete is
  skipped for the remaining paths instead of failing each one.
- File browser: folders show "not in source" too (all files gone from the
  source) or "N not in source" (some of them).
- File browser: "select all" selects every row with one click (before, each
  click added only one more row).

Peers: no.

## 1.11.0 (2026-10-30)

- Phone app (PWA): Settings → "App for your phone" installs BFBackup with
  its own icon and window (Android Chrome: Install app button or menu → Add
  to home screen). Needs HTTPS, e.g. through a reverse proxy. A service
  worker keeps the app shell for when the controller is unreachable; the API
  is never cached.
- Phones: new layout below 800 px. A bottom tab bar (Home, Jobs, History,
  Peers, More) replaces the sidebar; History and Logs show each row as a
  card; the file browser and run steps hide secondary columns; dialogs open
  as bottom sheets; form fields no longer zoom in on focus.
- Login: the session now lasts until it has not been used for 30 days
  (was: 7 days after login). The cookie is `Secure` behind an HTTPS proxy
  that sends `X-Forwarded-Proto: https`.
- New anomaly check "Many files would be removed (count)": stops when more
  than N files would be removed from a target, whatever its size. On by
  default with N = 50, also for existing installs. With both removal checks
  on, the lower limit wins.
- Settings: Global exclusions, Retention and limits, Activity log, Anomaly
  checks, Password and Controller backup are collapsed by default.
- File browser: a "select all" box in the header of the check column
  (also on the Not in source and Search tabs).
- Backup jobs: the hint below the title is gone.

Peers: no.

## 1.10.3 (2026-10-25)

- Deleting for good (and trashing across disks) tries again up to 5 times
  when a folder is "not empty" at the end, as happens on Unraid user shares
  when deleting uncovers a copy on another disk. If it still fails, the
  error names some of the files left behind.

Peers: yes.

## 1.10.2 (2026-10-25)

- Backup jobs: cards are packed into columns, so a job with several targets
  no longer leaves an empty gap next to it.
- Backup jobs: status, next run and size of a target stay on one line; the
  size no longer wraps onto a line of its own.
- Backup jobs: smaller online dots; "never backed up" is a red status pill.

Peers: no.

## 1.10.1 (2026-10-25)

- Separate test buttons in Settings for Pushover and email ("Save and send
  test push", "Save and send test email"); each saves the settings, then
  tests only its own channel.

Peers: no.

## 1.10.0 (2026-10-25)

- Email notifications by SMTP over TLS/SSL (usually port 465), next to
  Pushover: server, port, login, sender and recipients in Settings. Every
  channel that is set up gets each notification. STARTTLS is not supported.
- "Save and send test" saves the notification settings before testing them.
- Settings: Notifications and Trash are collapsed by default; the trash is
  only counted when opened.
- Trash help text no longer mentions replaced files.
- Job form: hooks moved to a new "Optional" section behind a "Use hooks"
  checkbox; unticking it removes the job's hooks.
- Job form: removed the help text of "Remove excluded files from the targets".
- Jobs overview: "never backed up" shows a red status light.

Peers: no.

## 1.9.0 (2026-10-25)

- Trash overview in Settings (all job folders, grouped by node) and on each
  job page (its own folders): files, size, oldest batch, when it is purged,
  and the batches with links to their runs.
- "Empty" button per folder removes its trash at once (type "empty" to
  confirm; written to the audit log).
- One trash batch per run (`<start>-run<id>`) instead of one folder per second.
- Run file log shows the newest lines first.
- Fix: "Create folder" in the folder picker reported success without creating
  a folder that had been deleted outside BFBackup.

Peers: yes (trash counting, batch names and the folder fix run on the peer).

## 1.8.1 (2026-10-24)

- Sizes in the web UI use decimal units (KB, MB, GB, TB), as Unraid does.

Peers: no.

## 1.8 (2026-10-24)

- Backups overwrite changed files in place, so the old copy no longer takes
  space in the trash.
- New jobs and new targets default to a manual schedule.
- Temp files are named `.BFBackup-*.tmp`; old `.bfb-*.tmp` leftovers are still
  recognised and cleaned up.

Peers: yes (temp file names).

## 1.7 (2026-10-24)

- Much clearer progress while a run works: live updates from peer scans
  (current folder, folders read), counts for removing files and anomaly
  checks, per-file copy progress, the latest log line on the run card.
- Reconnect waits are shown on the run card; the "no progress" warning comes
  after 3 minutes, names the step and says when the run will be stopped.
- Step timeline on the run page, also for finished runs.
- Folder picker can create a new folder; case-insensitive sorting in pickers
  and the file browser.
- "Replace only if the source is newer" is the default for new jobs.
- 24-hour times everywhere; disks hint text removed.

Peers: yes (scan progress and folder creation).

## 1.6.5 (2026-10-23)

- Backup jobs page redesigned: compact job cards with one line per target,
  a main button (back up / stop / continue), a ⋯ menu for the other actions,
  and a filter box with quick filters (needs you, running, disabled).

Peers: no.

## 1.6.4 (2026-10-23)

- Release build of 1.6.3, no further changes.

Peers: no.

## 1.6.3 (2026-10-22)

- Huge targets: the files to remove and the "not in source" list are read in
  one pass instead of thousands of slow pages.
- The dashboard and jobs page cache the "not in source" count, so the web UI
  stays fast.
- The run card names the steps after the scan (comparing, listing removals,
  anomaly checks, free space).
- Fix: a race in the copy step found by the race detector.

Peers: no.

## 1.6.2 (2026-10-22)

- Job queue: each peer takes part in one run at a time; other runs wait and
  start in order, and the dashboard says what each one waits for.
- A verify due together with a backup on the same target now queues instead
  of being skipped.
- Fix: a run's start time was the time it was queued.

Peers: no.

## 1.6.1 (2026-10-22)

- Scans log their phases and show live counters; the target check before the
  first backup is logged too.
- Checksum scans report the bytes read, also from peers, and the stall
  watchdog counts them as progress.
- A real stall shows "no progress for X" instead of a growing time estimate.
- Old log entries are pruned in small chunks; `/api/system/stacks` shows what
  every part is doing, for debugging hangs.
- Fix: the file log printed "null" for empty fields.

Peers: yes (checksum progress from peers).

## 1.6 (2026-10-22)

- Files already on a target (copied by hand or another tool) are taken over
  on the first backup, using the job's comparison; only missing files count
  for the free-space check.
- 2-second tolerance on modification times for every backup.
- Readable sizes in the "not enough space" message.
- Dry runs list which files a backup would remove, and why.

Peers: no.

## 1.5 (2026-10-22)

- Runs keep going after a peer reconnects; file transfers and scans are
  retried after a dropped connection.
- Large files (64 MB and up) resume where they stopped.
- Stalled transfers count as dropped connections and are retried.
- Longer keepalive for busy, slow links.
- zstd compression for file data and scan listings (`BFB_COMPRESS=off` turns
  it off).

Peers: yes (resume and compression need both sides).

## 1.4 (2026-10-22)

- A file transfer without data for 5 minutes fails and the run goes on.
- Stall watchdog: a run without progress for 30 minutes (Settings) is stopped.
- "Reconnect" button per peer on the Peers page.

Peers: recommended (connection code changed).

## 1.3 (2026-10-22)

- TLS encryption between controller and peers (`BFB_TRANSPORT`).
- Job types: copy or sync; per job what happens when a changed file exists on
  the target, and whether removed files go to the trash or are deleted.
- Global anomaly checks in Settings (history, mass removal, encryption,
  renames, smaller source, empty files, …) replace the per-job limit.
- Backups that need a decision wait on the dashboard and jobs page with
  Continue / Stop; failed runs can be dismissed.
- Log level 4: every step and decision, also each unchanged file.
- "Remove excluded files from the targets" is on by default.
- UI: "Backup history" in the sidebar, Logs below Settings, next backup and
  verify per target, no jobs table on the dashboard, audit log moved to Logs.

Peers: yes (encryption).

## 1.2 (2026-10-22)

- Errors say where they happened (source or which peer); old peers get a
  clear "update this peer" message.
- A permanent delete checks first that every node involved supports it.
- New job option: remove files on the targets that match the exclusions.
- The file log moved to the bottom of each run page.
- Unknown files on a target are just listed, without the long hint.

Peers: no.

## 1.1 (2026-10-22)

- Separate file log for per-file lines; the main log keeps events and errors.
- Verify lists unknown files on the target; backups list files gone from the
  source and skipped special files (devices, sockets, FIFOs).
- "Delete permanently (skip the trash)" option in the delete dialog.

Peers: yes (permanent delete).

## 1.0 (2026-10-22)

- First numbered version: controller and peers, incremental push/pull
  backups, verify, browse and restore, delete via the trash, schedules,
  exclusions, Pushover notifications and the web UI.
