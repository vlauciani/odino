# Contributing to odino

Thanks for your interest in improving odino. This document explains the
workflow and conventions used in this repository.

## Branching model (git-flow)

The repo follows [git-flow](https://nvie.com/posts/a-successful-git-branching-model/):

- `main` — production-ready code; every commit is a released (tagged) state.
- `develop` — integration branch for the next release.
- `feature/*` — new work, branched from and merged back into `develop`.
- `release/*` — release stabilization, branched from `develop`, merged into
  `main` and `develop`, then tagged `vX.Y.Z`.
- `hotfix/*` — urgent fixes branched from `main`.

Open pull requests against `develop` (never against `main` directly).

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/), in English,
imperative mood:

```
<type>: <description>
```

Allowed types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`,
`ci`, `perf`, `build`. Keep the first line under 72 characters.

## Before opening a pull request

Make sure the project builds and is clean:

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .          # should print nothing
```

CI runs the same checks plus `golangci-lint` on every push and pull request.

## Updating the changelog

Add a bullet under the current `*-dev` section of `CHANGELOG.md` describing the
user-visible change. Keep the entries terse and in English.

## Code style

- Keep the code idiomatic Go; match the surrounding style.
- Comments explain the *why*, not the *what*.
- No secrets, API keys, or tokens in code or configuration.
