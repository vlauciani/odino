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

## Cutting a release

The version lives in **`CHANGELOG.md`** only — as the `X.Y.Z-dev` token in the
top `### Release X.Y.Z-dev (date)` header. The binary version is injected at
build time from the git tag (GoReleaser ldflags), so no source file carries a
version to bump.

Releases are driven by a git-flow helper script that reads that version, strips
`-dev`, dates the header, runs `git flow release finish` (creating the `vX.Y.Z`
tag with the changelog block as its message), and opens the next `-dev` cycle:

```sh
# develop must already be pushed (the script pulls origin/develop first)
git push -u origin develop

# dry-run first
~/script/2026_git_flow_make_release/git-flow-release.sh \
  -p ~/gitwork/gitlab/_valentino.lauciani/odino -f CHANGELOG.md -n -c false

# then for real
~/script/2026_git_flow_make_release/git-flow-release.sh \
  -p ~/gitwork/gitlab/_valentino.lauciani/odino -f CHANGELOG.md -c false
```

Pushing the `vX.Y.Z` tag triggers the `release` workflow, which publishes the
per-platform binaries to the GitHub Release and the multi-arch image to Docker
Hub. The release job needs the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` repo
secrets.

## Code style

- Keep the code idiomatic Go; match the surrounding style.
- Comments explain the *why*, not the *what*.
- No secrets, API keys, or tokens in code or configuration.
