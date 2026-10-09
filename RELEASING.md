# Releasing gomutants

Maintainer runbook. Releases are cut by pushing an annotated tag from a local
clone — there is no release-from-the-UI path and no scheduled release job.

## Versioning

| Form | Example | Published as |
| --- | --- | --- |
| Stable | `v0.5.1` | Normal release, becomes the repo's **Latest** |
| Release candidate | `v0.5.1-rc0` | **Pre-release**, never becomes Latest |

Only these forms produce releases. Tags matching the workflow's `v*.*.*`
trigger but not the accepted pattern (for example, `v1.0.0-alpha`) fail the
*Validate semver tag* step. Tags outside that trigger (for example, `v0.6` or
`v1.0-alpha`) do not start the workflow. There is no `-rc.1`, `-alpha`, or
`-beta` support; the accepted pattern is
`^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$`.

## What the pipeline does for you

Pushing a matching tag runs `.github/workflows/release.yml`:

| Job | What it does |
| --- | --- |
| `verify` | Validates the tag, runs `go test -race ./...` and `go vet ./...`, then builds with the release ldflags and asserts `--version` echoes the version, commit, and build date |
| `release` | Resolves the changelog boundary, runs GoReleaser (cross-builds, archives, checksums, SPDX SBOMs, cosign keyless signing), and attests build provenance |
| `provenance` | Generates SLSA Level 3 provenance via `slsa-github-generator` |
| `publish` | Uploads the provenance attestation and flips the release out of draft |

The release exists as a **draft** until `provenance` succeeds, so a failure
partway through never leaves a half-published release visible to users.

Release candidates go through this pipeline unchanged — same signing, same
SBOMs, same provenance. The only difference is that GoReleaser marks them as
pre-releases (`prerelease: auto` in `.goreleaser.yaml`) and the `publish` job
passes `--latest=false`.

## Cutting a release candidate

```bash
git checkout main && git pull --ff-only
git tag -a v0.5.1-rc0 -m "v0.5.1-rc0"
git push origin v0.5.1-rc0
```

An rc's release notes cover only the delta since the previous tag — the last
rc, or the last stable for `rc0`. Do **not** bump the README version pins for
an rc; those track stable releases only.

## Testing a release candidate

Pin both the action ref and the `version` input:

```yaml
- uses: gomutants/gomutants@v0.5.1-rc0
  with:
    version: v0.5.1-rc0
    args: --changed-since origin/main ./...
```

`version: latest` resolves to the newest **stable** release and will not pick
up an rc. In the job log, confirm the step prints `Verifying build provenance`
— if it instead warns about *falling back to 'go install'*, the rc took the
unverified path and something in `action.yml`'s tag matching is wrong.

## Promoting to stable

Before pushing, preview the changelog boundary. It must print the previous
**stable** tag; if it prints an rc, the *Resolve previous stable tag* step in
the release workflow is broken — investigate rather than pushing.

```bash
TAG=v0.5.1
prev="$(git tag --list 'v*.*.*' --merged HEAD --sort=-v:refname \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vxF "$TAG" | head -n 1)"
echo "$prev"
git log --oneline "$prev..HEAD"
```

Then tag and push:

```bash
git tag -a v0.5.1 -m "v0.5.1"
git push origin v0.5.1
```

A stable release's notes span back to the previous stable tag, so they include
everything that shipped through the intervening rcs.

## Manual steps after a stable tag

1. **Check the release page.** Four `tar.gz` archives, `checksums.txt` plus its
   `.sigstore.json` bundle, a `.spdx.json` per archive, and
   `multiple.intoto.jsonl`. The "Latest" badge should have moved; any rc
   releases should still read "Pre-release".

2. **Bump the README pins.** `README.md` pins the action and `go install`
   examples to a commit SHA with a trailing version comment
   (`@<sha> # v0.5.0`). Update both to the tagged commit:

   ```bash
   git rev-list -n1 v0.5.1
   ```

   Commit as `docs: pin README examples to v0.5.1`.

   **First stable release after the module rename only:** releases up to
   v0.6.1 declare `github.com/szhekpisov/gomutants` in `go.mod`, so the
   `go install` / `go run` examples and the pkg.go.dev badge still use that
   path. Once a stable release declares `github.com/gomutants/gomutants`,
   `@latest` only works with the new path. In the same commit, switch them in
   `README.md`, `plugin/README.md` and `plugin/commands/mutants.md`, then
   delete this note.

3. **Smoke-test the published artifact.**

   ```bash
   gh release download v0.5.1 --pattern 'gomutants_0.5.1_linux_amd64.tar.gz'
   gh attestation verify gomutants_0.5.1_linux_amd64.tar.gz --repo gomutants/gomutants
   ```

## If a release fails midway

- **`verify` failed** — nothing was published. Delete the tag locally and on
  the remote, fix the problem, and re-tag.
- **`release` or `provenance` failed** — the GitHub release exists as a draft.
  GoReleaser will not overwrite an existing release, so delete both the draft
  and the tag before retrying:

  ```bash
  gh release delete v0.5.1 --yes --cleanup-tag
  git tag -d v0.5.1
  ```

Because published tags are immutable, never re-point a tag that already made it
through `publish` — cut the next patch or rc instead.
