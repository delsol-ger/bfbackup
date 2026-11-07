# Contributing

Bug reports and ideas are welcome as issues. Please include the versions of
controller and peers.

Pull requests: open an issue first for anything larger than a small fix, so
we can agree on the approach. Keep the plain-files design: backups must stay
readable without BFBackup.

```bash
make test   # all tests (uses Go in Docker if Go is not installed)
make race   # with the race detector
make docker # build the image
```

The code map is in [docs/DESIGN.md](../docs/DESIGN.md). By contributing you
agree that your code is published under the GPL-3.0.
