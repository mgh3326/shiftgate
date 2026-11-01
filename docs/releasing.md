# Releasing shiftgate

## Cut a release

1. Merge the change to `main`.
2. Tag the merge commit and push the tag:
   ```sh
   git tag vMAJOR.MINOR.PATCH origin/main
   git push origin vMAJOR.MINOR.PATCH
   ```
3. `.github/workflows/release.yml` builds static (`CGO_ENABLED=0`,
   `-trimpath`) binaries named `shiftgate-<os>-<arch>`. It builds four targets: linux/amd64,
   linux/arm64, darwin/amd64 and darwin/arm64. It stamps the tag as the
   version and writes `SHA256SUMS`. It checks that `shiftgate version`
   prints the tag. It runs the tests. Then it publishes the GitHub Release
   with `GITHUB_TOKEN`.

Every pull request runs the same build and checks for the dry-run tag
`v0.0.0-dryrun.<run>`, and publishes nothing. To run the same check locally:

```sh
sh scripts/build-release.sh v0.0.0-dryrun.1 dist/release
sh scripts/verify-release.sh v0.0.0-dryrun.1 dist/release
```

A tag with a `-suffix` (`v1.2.3-rc.1`) builds the same way. Mark that release
as a *pre-release* on GitHub, because hosts that follow `latest` skip
pre-releases.

## How hosts get it

No workflow connects to a host. On each host, the fleet updater
(`fleet-update`) reads the latest release, or the tag pinned for that host.
It verifies the asset against `SHA256SUMS` and swaps the binary atomically.
It keeps the old binary as `shiftgate.prev` and restarts the owning service. If
the version or service check fails, it restores `shiftgate.prev`.

## Pin, hold, roll back

- **Pin or hold:** set `version=vX.Y.Z` on the host's line in the fleet-update
  manifest. Remove the pin to follow `latest` again.
- **Roll back the fleet:** pin the previous good tag. Alternatively, mark the
  bad release as a pre-release or delete it, so that `latest` points to the
  previous release on the next run.
- **Roll back one host now:** copy `shiftgate.prev` over `shiftgate`, restart the
  service, then pin the tag so that the next run does not reinstall the bad
  release.
