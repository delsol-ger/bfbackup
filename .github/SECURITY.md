# Security

## Reporting a problem

Please **don't open a public issue** for security problems. Report them
privately: **Security → Report a vulnerability** on
https://github.com/delsol-ger/bfbackup/security/advisories/new.

Say what you found, how to reproduce it and which version you use. You get
an answer within a week. Fixes are released as a new version; the changelog
names security fixes as such.

Only the latest release gets fixes.

## What BFBackup protects, and what not

- **Peer connections**: TLS 1.3 by default; peers and controller prove they
  know the token secret with HMAC challenge-response bound to the TLS
  session, so a man in the middle fails without any certificates. The secret
  never crosses the network. Unencrypted mode (`BFB_TRANSPORT=plain`) can be
  read and altered by anyone on the path.
- **Web UI**: one admin user, bcrypt, `HttpOnly` + `SameSite=Strict`
  session cookie, CSRF header, login rate limit, strict CSP. It is made for
  the LAN or a VPN. **Never forward port 7700 to the internet**, also not
  through a reverse proxy.
- **File access**: BFBackup only touches the folders in `BFB_ROOTS`
  (symlinks included). The Unraid templates map all of `/mnt` read/write,
  because moving deleted files into the trash on the same disk needs the
  single disks. Narrow `BFB_ROOTS` on peers to the backup folder, so the
  controller cannot see anything else there.
- **Runs as root** inside the container, so it can read every file it backs
  up and set the owner (`BFB_UID`/`BFB_GID`) of the files it writes. It is
  not privileged and needs no extra capabilities.
- **Hooks**: the controller picks a script by file name from
  `/config/hooks/` on the source node; it can never send commands.
- **Not encrypted at rest**: backups are plain files on purpose. Protect
  the target disks like the originals (disk encryption, physical access).
- The **configuration export** contains the peer tokens. Keep it safe.
