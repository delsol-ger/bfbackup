# Unraid templates

Normally you install BFBackup from **Apps** (Community Applications): search
for **BFBackup**. These are the same templates, for installing by hand.

| File | Install on | Purpose |
|---|---|---|
| `bfbackup-controller.xml` | exactly one server | web UI, schedules, database |
| `bfbackup-peer.xml` | every other server that sends or keeps backups | receives or sends files for the controller |

Both use the same image, `delsolger/bfbackup`. The role is the container
argument (`controller` or `peer`, under *Post Arguments*).

## Install without the App Store

On the Unraid console of the server:

```bash
wget -O /boot/config/plugins/dockerMan/templates-user/my-bfbackup-controller.xml \
  https://raw.githubusercontent.com/delsol-ger/bfbackup/main/public-release/unraid/bfbackup-controller.xml
```

```bash
wget -O /boot/config/plugins/dockerMan/templates-user/my-bfbackup-peer.xml \
  https://raw.githubusercontent.com/delsol-ger/bfbackup/main/public-release/unraid/bfbackup-peer.xml
```

Then **Docker → Add Container**, pick the template, fill in the fields and
apply. The main [README](../../README.md#install-on-unraid) explains the
first steps (password, adding a peer, the first job).
