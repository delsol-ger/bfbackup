# Roadmap and feature requests

What is requested, planned or decided against. Shipped features move to
[CHANGELOG.md](CHANGELOG.md) with the version they came in.

Have an idea? Open a
[feature request](https://github.com/delsol-ger/bfbackup/issues/new?template=feature_request.yml).
All open requests:
[issues labeled `enhancement`](https://github.com/delsol-ger/bfbackup/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement).

## In progress

_Nothing right now._

## Planned

_Nothing confirmed yet._

## Requested / under consideration

* **Versioning** (keep old versions of changed and deleted files). The place
  to add it is described in [docs/DESIGN.md](docs/DESIGN.md).
* **Catch-up of missed schedules**: run jobs whose time passed while the
  controller was off.
* **2FA login** for the web UI (one-time code from an authenticator app).
* **Several users** for the web UI.
* Extend script support before and after backup job run.

## Not planned

* **Encryption at rest.** Backups are plain files on purpose, so they can be
  read without BFBackup. Use disk encryption on the target instead (see
  [Security](README.md#security)).
