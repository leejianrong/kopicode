# Contributing to kopicode

Thanks for helping. This is the human-facing setup and workflow; the structural rules every
change must respect are in [`AGENTS.md`](AGENTS.md) (read its "Boundaries that must not be
crossed"), and the reasoning behind the design is in [`docs/adr/`](docs/adr/README.md).

## Set up

You need Go 1.26.9 (the version `go.mod` pins), `make`, and `git`.

```bash
git clone https://github.com/leejianrong/kopicode.git && cd kopicode
make dev            # downloads modules and installs golangci-lint, gitleaks and friends
make install-hooks  # a pre-push hook running the cheap gates
```

Install Go from the [official tarball](https://go.dev/dl/) and put its `bin` directory on
your `PATH`. **Do not export `GOROOT`**: the `go` binary finds its own root, and a stale
export (an old `/usr/local/go`, say) makes every Go command fail with "cannot find GOROOT
directory". `make dev` installs `golangci-lint` and `gitleaks` into `~/go/bin`, which must
be on `PATH` too; CI pins golangci-lint v2.12.2 and gitleaks v8.30.1.

No API key is needed to develop: the default test loop uses a mock provider and costs
nothing.

## Before you push

```bash
make check          # gofmt, vet, golangci-lint, go.mod tidy, installer tests
make test           # go test -short -race -count=1 ./...
make ci             # everything CI runs offline: check, test-all, bench-smoke, xbuild
```

The pre-push hook is the cheap subset, so a green hook does not mean green CI; run `make ci`
when a change touches more than one package. `make bench` runs the benchmark against the real
provider and **costs money** (about $0.08); `make bench-smoke` is the free equivalent and is
what gates a PR. Never commit a credential: `make secrets` scans the tree and history.

## Workflow

- `main` is protected. Branch off `origin/main` (`git switch -c feat/<name>`), push, and open
  a pull request. Every PR needs all seven CI checks green; branch protection is strict, so a
  PR that falls behind `main` is updated and re-run before it merges.
- Commits and PRs are authored by you alone: no `Co-Authored-By` or "Generated with" trailers.
- A change to behaviour updates the docs it touches in the same PR: the README, the protocol
  document for the surface it changes, or the ADR. A change that overturns a decision needs
  an ADR amending the old one first.
- Add a test with the change. A guard that has never been seen to fail may be asserting
  nothing, so break it on purpose once, watch the test go red, and put the output in the PR.
- Adding a dependency needs a reason in the PR description. The list is deliberately short
  and there is no CGo.

## Reporting a problem

Open an issue at <https://github.com/leejianrong/kopicode/issues>. For a bad session, the
journal under `.kopicode/sessions/<id>/` is the record of exactly what happened; attach the
relevant part, after checking it for anything you would not want public.
