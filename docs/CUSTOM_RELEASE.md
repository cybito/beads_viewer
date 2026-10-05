# Custom release provenance

- GitHub fork: `https://github.com/cybito/beads_viewer` (upstream: `https://github.com/Dicklesworthstone/beads_viewer.git`). `custom` is the custom build and release source.
- Historical Forgejo source: `https://git.cybit.top/cybit/beads_viewer`; custom ref at migration: `2faac1cf39f1a2077e59dc20ecfaf51ffc42a313`. It remains intact.
- Historical OCI native artifact `git.cybit.top/cybit/ias-bv`: Darwin digest `sha256:448fd26cab31268768e2321e7d5e64483108de5aa7d75bdbfb0e46496342943d`; Linux digest `sha256:5848850f6f91c54b5064459f4d63f1b076d8d1db71a7fa8c9003e7f8c06bbf19`. These original binary/receipt artifacts remain unchanged.

## Publish a custom release

Source development and release tags live on the public GitHub fork. Only
`.github/workflows/custom-release.yml` should be enabled in the fork. Inherited
upstream workflows remain in the tree but must be disabled in GitHub. The
fork's default branch must be `custom`.

Publishing a GitHub Release triggers the workflow; an ordinary push (including
a tag push), draft release, or pull request does not publish packages. The tag
must be `v<major>.<minor>.<patch>-custom.<positive integer>`, its version base
must match `pkg/version/version.go`, and its dereferenced commit must be an
ancestor of `origin/custom`. This migration's first asset-bearing release uses
`v0.25.0-custom.5`; existing tags/releases are never moved. For example:

```sh
sha=$(git rev-parse custom)
gh release create v0.25.0-custom.5 \
  --repo cybito/beads_viewer --target "$sha" \
  --title v0.25.0-custom.5 \
  --notes 'Custom bv source on GitHub; ARM64 installation packages are attached below.'
```

Use the workflow's built-in `GITHUB_TOKEN` (`GH_TOKEN`) and `contents: write`
permission to publish Release assets; no Forgejo PAT, environment, or registry
credentials are needed. Builds use the same validated exact SHA on `macos-26`
and `ubuntu-24.04-arm`, with Go **1.26.8**, `GOTOOLCHAIN=local`, `GOWORK=off`,
`CGO_ENABLED=0`, `-mod=vendor`, and the existing vendor/third_party
replacements. There is no `go mod tidy`, GoReleaser upload, GitHub Actions
artifact upload, or Actions cache. `bv --version` reports the complete release
tag. The native ARM64 executable's Go metadata must record that SHA with
`vcs.modified=false`. A disposable HOME/config and two-issue `.beads` fixture
verify that `--robot-triage` recommends SMOKE-1 and lists SMOKE-2 as unblocked
by it; no production tracker or browser is started. The installer is exercised
twice both before and after archive packing.

## Release asset contract

Product files are attached directly to the matching GitHub Release. Every
asset is deterministically named
`<release-tag>-<platform>-<original-package-filename>`, including `release.json`
and `SHA256SUMS`; the Darwin and Linux sets therefore cannot collide. The
archive retains its normal package layout and receipt records source tag,
commit, platform, payload filename, size and SHA256. Checksums cover both the
archive and receipt.

Before building, the helper lists assets with `gh release view --json assets`
and downloads an existing platform set with `gh release download`. A complete
set is reused only after downloaded bytes pass receipt identity, payload hash,
checksum, and layout validation. A partial set triggers a rebuild and is
safely reconciled: existing names are checked byte-for-byte and only missing
names are uploaded. Mismatching existing bytes fail, identical bytes are reused,
and only missing names are uploaded. Upload uses no `--clobber`, so it never
overwrites assets. The helper enforces GitHub's per-file size below 2 GiB and
1000-assets-per-release limits before uploading. It reads assets back and
validates their bytes after upload. If one platform upload succeeds and the
other fails, a rerun can safely complete the missing platform without changing
the first set.

After both platforms succeed, the workflow updates only the managed
`<!-- custom-builds:start -->` / `<!-- custom-builds:end -->` block of the
original Release notes, preserving all user-authored notes. The block links
each downloadable Release asset. Failed runs do not claim both platforms are
available; verified platform assets are listed in job summaries.

Download and install, for example:

```sh
mkdir -p /absolute/path/bv-download
cd /absolute/path/bv-download
gh release download v0.25.0-custom.5 --repo cybito/beads_viewer \
  --pattern 'v0.25.0-custom.5-linux-*'
for file in v0.25.0-custom.5-linux-*; do
  mv "$file" "${file#v0.25.0-custom.5-linux-}"
done
sha256sum -c SHA256SUMS
mkdir unpacked
tar -xzf bv-v0.25.0-custom.5-linux-arm64.tar.gz -C unpacked
./unpacked/install.sh --prefix /absolute/path/chosen-prefix
/absolute/path/chosen-prefix/bin/bv --version
```

For macOS, download the Darwin asset set, strip its tag/platform filename
prefix in the same way, and use `shasum -a 256 -c SHA256SUMS` instead.
The default install prefix, if omitted, is `$HOME/.local`. Only the given
prefix's `bin` and `share/bv` are written. No services are restarted, user
configuration overwritten, or system packages removed. An existing different
file or symlink destination is rejected before copying; identical files are
left unchanged, making a second install idempotent. Keep the chosen prefix's
`bin` on PATH yourself. macOS packages are not Apple Developer notarized; local
Gatekeeper policy may require manual approval.

### Helper interfaces and verification hand-off

All helper paths must be absolute. Project and repository are fixed in the
script; callers cannot redirect publication to another owner.

```sh
python3 .github/scripts/package-release.py check \
  --tag TAG --commit SHA --platform darwin --output-dir /absolute/empty/check
bash .github/scripts/custom-release.sh build \
  darwin TAG SHA /absolute/nonexistent/build
python3 .github/scripts/package-release.py pack \
  --tag TAG --commit SHA --platform darwin \
  --input-dir /absolute/build --output-dir /absolute/empty/package
python3 .github/scripts/package-release.py publish \
  --directory /absolute/package
```

`check` emits JSON with `exists` and, when present, the downloaded asset names;
`pack` emits `directory`; `publish` reports uploaded/reused names after a
successful read-back verification. GitHub CLI authentication is supplied by
the workflow token, not by a Forgejo credential. The workflow runs isolated
tag-ancestry, invalid-tag, shell-injection, conflicting-asset, partial-set,
corrupt-payload, checksum, and no-overwrite regressions via
`bash .github/scripts/custom-release.sh validation-regressions`. Delivery
verification must additionally observe both hosted jobs, repeat a successful
run without byte changes, download and install each platform set into
disposable prefixes, and confirm ordinary pushes do not trigger this workflow.
Source editing alone is not evidence of a successful hosted build or upload.
