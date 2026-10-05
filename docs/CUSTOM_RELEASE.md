# Custom release provenance

- GitHub fork: `https://github.com/cybito/beads_viewer` (upstream: `https://github.com/Dicklesworthstone/beads_viewer.git`). `custom` is the custom build and release source.
- Historical Forgejo source: `https://git.cybit.top/cybit/beads_viewer`; custom ref at migration: `2faac1cf39f1a2077e59dc20ecfaf51ffc42a313`. It remains intact.
- Historical OCI native artifact `git.cybit.top/cybit/ias-bv`: Darwin digest `sha256:448fd26cab31268768e2321e7d5e64483108de5aa7d75bdbfb0e46496342943d`; Linux digest `sha256:5848850f6f91c54b5064459f4d63f1b076d8d1db71a7fa8c9003e7f8c06bbf19`. These original binary/receipt artifacts remain unchanged.
- Custom install packages are a distinct OCI artifact type in `ias-bv`. Immutable download: `oras pull git.cybit.top/cybit/ias-bv@sha256:<digest>`.

## Publish a custom release

Source development and release tags live on the public GitHub fork; Forgejo is
the historical source archive and OCI package store, not a source push mirror.
Only `.github/workflows/custom-release.yml` should be enabled in the fork.
Inherited upstream workflows remain in the tree but must be disabled in GitHub.
The fork's default branch must be `custom`.

Publishing a GitHub Release triggers the workflow; an ordinary push (including
a tag push), draft release, or pull request does not publish packages. The tag
must be `v<major>.<minor>.<patch>-custom.<positive integer>`, its version base
must match `pkg/version/version.go`, and its dereferenced commit must be an
ancestor of `origin/custom`. The first planned release is
`v0.25.0-custom.3`; if occupied, choose the next unused custom integer rather
than modifying an existing tag. For example, after committing and pushing all
three CI files and this documentation:

```sh
sha=$(git rev-parse custom)
gh release create v0.25.0-custom.3 \
  --repo cybito/beads_viewer --target "$sha" \
  --title v0.25.0-custom.3 \
  --notes 'Custom bv source on GitHub; ARM64 installation packages in Forgejo OCI.'
```

Do not attach files to the GitHub Release. Builds use the same validated exact
SHA on `macos-26` and `ubuntu-24.04-arm`, with Go **1.26.8**, `GOTOOLCHAIN=local`,
`GOWORK=off`, `CGO_ENABLED=0`, `-mod=vendor`, and the existing vendor/third_party
replacements. There is no `go mod tidy`, GoReleaser upload, GitHub artifact
upload, or Actions cache. `bv --version` reports the complete release tag.
The native ARM64 executable's Go metadata must record that SHA with
`vcs.modified=false`. A disposable HOME/config and two-issue `.beads` fixture
verify that `--robot-triage` recommends SMOKE-1 and lists SMOKE-2 as unblocked
by it; no production tracker or browser is started. The installer is exercised
twice both before and after archive packing.

### Registry credentials and package association

A human must supply a new dedicated Forgejo PAT named `github-custom-builds`
for `cybit`, with package-write permission, preferably public-only. This
permission can reach other packages in the same owner namespace; it is **not**
limited to `ias-bv` or these six repositories. Never export existing fj OAuth
keys, Docker helper credentials, or user passwords. Put the PAT into the
`FORGEJO_REGISTRY_TOKEN` secret of GitHub environment `forgejo-registry`; its
deployment policy must permit tags `v*-custom.*`, not ordinary branches.
Without that environment and secret, a first registry publication is blocked.
The token is supplied only to the upload step, after build/installation smoke;
ORAS auth uses an isolated mode-0600 file in a mode-0700 runner temporary
directory, removed on success or failure.

The Forgejo package settings must associate `ias-bv` with
`cybit/beads_viewer`. OCI source annotations still truthfully identify
`https://github.com/cybito/beads_viewer.git`; do not rewrite them to trick
Forgejo's first-package auto-link behavior.

## OCI package and installation

New artifacts use `application/vnd.cybito.install-package.v1`, under the
existing `git.cybit.top/cybit/ias-bv` namespace. Old
`application/vnd.ias.native.v1` binaries, receipts, source identity and digests
listed above remain unchanged. This migration does not modify IaC pins or
deploy anything.

Tags are `<release-tag>-darwin-arm64` and `<release-tag>-linux-arm64`.
Each artifact contains:

- `bv-<release-tag>-<platform>-arm64.tar.gz`: `bin/bv`,
  `share/bv/{LICENSE,README.md,build-metadata.txt}`, any top-level NOTICE, and
  `install.sh`.
- `release.json`: schema 1 with `project`, `source_repo`, `source_commit`,
  `release_tag`, `platform`, `architecture`, `toolchains`, and each archive's
  filename, SHA256 and size. It is a separate OCI layer, never recursively
  embedded in the archive.
- `SHA256SUMS`: covers the archive and `release.json`.

The OCI layer media types are `application/gzip`, `application/json`, and
`text/plain`, respectively. The manifest's created time is the source commit's
UTC time. Publication constructs a local OCI layout, determines its digest,
copies it to Forgejo, and fetches every descriptor plus an independent pull
to validate manifest bytes, layer bytes and checksums. An existing complete
tag with the same source/tag/platform is verified and reused without
recompiling; different identities and corrupt files fail. Only explicit
registry `MANIFEST_UNKNOWN`/`NAME_UNKNOWN` means absent—authentication,
network and other errors fail, rather than authorizing a replacement.
Each platform has its own non-cancelling concurrency lock. A failed platform
does not remove the other's successful artifact; rerun to fill the missing
platform.

After both platforms succeed, an anonymous verification job updates only the
`<!-- custom-builds:start -->` / `<!-- custom-builds:end -->` block of the
original Release notes, preserving all other user notes. It records immutable
digest references and download commands. Failed runs do not claim a complete
download table; individual verified platform digests remain in job summaries.

Install from the digest reference in those notes (ORAS **1.3.3**):

```sh
mkdir -p /absolute/path/bv-download
oras pull git.cybit.top/cybit/ias-bv@sha256:<digest> \
  --output /absolute/path/bv-download
cd /absolute/path/bv-download
# Linux:
sha256sum -c SHA256SUMS
# macOS instead: shasum -a 256 -c SHA256SUMS
mkdir unpacked
tar -xzf bv-v0.25.0-custom.3-linux-arm64.tar.gz -C unpacked
# Use the corresponding darwin archive on Apple Silicon.
./unpacked/install.sh --prefix /absolute/path/chosen-prefix
/absolute/path/chosen-prefix/bin/bv --version
```

The default prefix, if omitted, is `$HOME/.local`. Only the given prefix's
`bin` and `share/bv` are written. No services are restarted, user configuration
overwritten, or system packages removed. An existing different file or
symlink destination is rejected before copying; identical files are left
unchanged, making a second install idempotent. Keep the chosen prefix's `bin`
on PATH yourself. macOS packages are not Apple Developer notarized; local
Gatekeeper policy may require manual approval.

### Helper interfaces and verification hand-off

All helper paths must be absolute. Project, owner, registry, and package are
fixed in the repository; callers cannot redirect publication to another owner.

```sh
python3 .github/scripts/package-release.py check \
  --tag TAG --commit SHA --platform darwin --output-dir /absolute/empty/check
bash .github/scripts/custom-release.sh build \
  darwin TAG SHA /absolute/nonexistent/build
python3 .github/scripts/package-release.py pack \
  --tag TAG --commit SHA --platform darwin \
  --input-dir /absolute/build --output-dir /absolute/empty/package
python3 .github/scripts/package-release.py publish \
  --directory /absolute/package --registry-config /absolute/private/config.json
python3 .github/scripts/package-release.py verify \
  --reference git.cybit.top/cybit/ias-bv@sha256:<digest> \
  --output-dir /absolute/empty/verified
```

`check` emits JSON with `exists` and, when present, immutable `reference`;
`pack` emits `directory`; `publish` emits `reference` and `digest`; `verify`
emits the checked receipt. `check` and `verify` always use explicit isolated
anonymous registry configuration. `publish` takes an explicit auth file and
never discovers credentials from the user's Docker/ORAS configuration.

The workflow runs isolated tag-ancestry, invalid-tag, shell-injection,
conflicting-receipt, corrupt-payload, and registry-error regressions via
`bash .github/scripts/custom-release.sh validation-regressions`. Delivery
verification must additionally observe both real hosted jobs, repeat a
successful run without digest changes, pull each immutable package, install
into disposable prefixes, confirm Release assets remain empty, confirm no new
GitHub artifacts/caches, and confirm ordinary pushes do not trigger this
workflow. Source editing alone is not evidence of a successful hosted build
or registry upload.
