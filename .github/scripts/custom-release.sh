#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == validate-release ]]; then
  python3 - "$GITHUB_EVENT_PATH" <<'PY'
import json, os, pathlib, re, subprocess, sys
run = lambda *a: subprocess.check_output(a, text=True).strip()
event = json.loads(pathlib.Path(sys.argv[1]).read_text())
release = event['release']
if event['repository']['full_name'] != 'cybito/beads_viewer' or release['draft'] or event.get('action') != 'published':
    raise SystemExit('not an authorized published release')
tag = release['tag_name']
if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*', tag):
    raise SystemExit('invalid custom release tag')
sha = run('git', 'rev-parse', '--verify', 'refs/tags/' + tag + '^{commit}')
if run('git', 'rev-parse', 'HEAD') != sha:
    raise SystemExit('checkout does not match exact release tag commit')
subprocess.run(['git', 'merge-base', '--is-ancestor', sha, 'refs/remotes/origin/custom'], check=True)
for name in ('.github/workflows/custom-release.yml', '.github/scripts/custom-release.sh', '.github/scripts/package-release.py'):
    subprocess.run(['git', 'cat-file', '-e', sha + ':' + name], check=True)
version = run('git', 'show', sha + ':pkg/version/version.go')
base = re.search(r'const fallback = "(v[0-9]+\.[0-9]+\.[0-9]+)"', version)
if not base or tag.split('-custom.')[0] != base.group(1):
    raise SystemExit('tag base does not match native version')
with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
    output.write('sha=' + sha + '\ntag=' + tag + '\n')
PY
  exit
fi

if [[ ${1:-} == validation-regressions ]]; then
  python3 - "$(pwd)/.github/scripts/custom-release.sh" <<'PY'
import hashlib, importlib.util, json, os, pathlib, subprocess, sys, tempfile
from unittest.mock import patch
script = sys.argv[1]
with tempfile.TemporaryDirectory() as tmp:
    root = pathlib.Path(tmp)
    def git(*args):
        return subprocess.check_output(['git', *args], cwd=root, text=True).strip()
    git('init', '-q')
    git('config', 'user.name', 'fixture')
    git('config', 'user.email', 'fixture@example.invalid')
    for name in ('.github/workflows/custom-release.yml', '.github/scripts/custom-release.sh', '.github/scripts/package-release.py'):
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text('fixture\n')
    p = root / 'pkg/version/version.go'
    p.parent.mkdir(parents=True)
    p.write_text('const fallback = "v0.25.0"\n')
    git('add', '.')
    git('commit', '-qm', 'custom ancestor')
    ancestor = git('rev-parse', 'HEAD')
    git('update-ref', 'refs/remotes/origin/custom', ancestor)
    git('tag', 'v0.25.0-custom.1')
    (root / 'other').write_text('upstream-only')
    git('add', '.')
    git('commit', '-qm', 'not in custom')
    git('tag', 'v0.25.0-custom.2')
    event = {'action':'published','repository':{'full_name':'cybito/beads_viewer'},'release':{'draft':False,'tag_name':'v0.25.0-custom.1'}}
    event_path = root / 'event.json'
    env = dict(os.environ, GITHUB_EVENT_PATH=str(event_path), GITHUB_OUTPUT=str(root / 'output'))
    for tag, success in [('v0.25.0-custom.1', True), ('v0.25.0-custom.2', False), ('v0.25.0', False), ('v0.25.0-custom.1;touch INJECTED', False)]:
        if tag in ('v0.25.0-custom.1', 'v0.25.0-custom.2'):
            git('checkout', '--detach', '-q', tag)
        event['release']['tag_name'] = tag
        event_path.write_text(json.dumps(event))
        result = subprocess.run(['bash', script, 'validate-release'], cwd=root, env=env, capture_output=True)
        if (result.returncode == 0) != success or (root / 'INJECTED').exists():
            raise SystemExit('release validation regression: ' + tag)
    event['release']['tag_name'] = 'v0.25.0-custom.1'
    event_path.write_text(json.dumps(event))
    git('checkout', '--detach', '-q', 'v0.25.0-custom.2')
    result = subprocess.run(['bash', script, 'validate-release'], cwd=root, env=env, capture_output=True)
    if result.returncode == 0:
        raise SystemExit('mismatched checkout was accepted')

spec = importlib.util.spec_from_file_location('release_package', pathlib.Path(script).with_name('package-release.py'))
package = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package)
def fixture(root, platform='linux', content=b'archive fixture'):
    root.mkdir()
    tag, commit = 'v0.25.0-custom.1', 'a' * 40
    name = f'bv-{tag}-{platform}-arm64.tar.gz'
    (root / name).write_bytes(content)
    receipt = package.identity(tag, commit, platform)
    receipt.update(toolchains={'go':'go1.26.8'}, files=[{'name':name,'size':len(content),'sha256':hashlib.sha256(content).hexdigest()}])
    (root / 'release.json').write_text(json.dumps(receipt))
    (root / 'SHA256SUMS').write_text(''.join(package.hash_file(root / n) + '  ' + n + '\n' for n in sorted([name, 'release.json'])))
    return receipt

def mocked_download(assets, contents):
    def download(tag, names, output):
        output.mkdir(parents=True, exist_ok=True)
        for name in names:
            (output / name).write_bytes(contents[name])
    return download
with tempfile.TemporaryDirectory() as tmp:
    root = pathlib.Path(tmp)
    pkgdir = root / 'package'
    receipt = fixture(pkgdir)
    files = package.expected_assets(pkgdir, receipt)
    records = {name:{'name':name,'size':path.stat().st_size} for name,path in files.items()}
    contents = {name:path.read_bytes() for name,path in files.items()}
    assert package.validate(pkgdir) == receipt
    archive = next(pkgdir.glob('bv-*.tar.gz'))
    original = archive.read_bytes()
    archive.write_bytes(b'corrupt archive')
    try: package.validate(pkgdir)
    except ValueError: pass
    else: raise SystemExit('corrupt archive was accepted')
    archive.write_bytes(original)
    with patch.object(package, 'release_assets', return_value={}), patch.object(package, 'download_assets'):
        assert package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'missing') == {'exists':False}
    partial = dict(list(records.items())[:1])
    with patch.object(package, 'release_assets', return_value=partial):
        assert package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'partial') == {'exists':False, 'partial':True}
    unexpected = dict(records)
    unexpected[receipt['release_tag'] + '-linux-extra.bin'] = {'name':'extra','size':1}
    with patch.object(package, 'release_assets', return_value=unexpected):
        try: package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'unexpected')
        except ValueError: pass
        else: raise SystemExit('unexpected same-platform asset was accepted')
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, contents)):
        assert package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'full')['exists']
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, contents)):
        try: package.check(receipt['release_tag'], 'b' * 40, 'linux', root/'identity-mismatch')
        except ValueError: pass
        else: raise SystemExit('assets with conflicting source commit were reused')
    bad = dict(contents); bad[next(iter(bad))] = b'mismatching bytes'
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, bad)):
        try: package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'mismatch')
        except (ValueError, RuntimeError): pass
        else: raise SystemExit('mismatching asset bytes accepted')
    bad = dict(contents); bad[next(name for name in bad if name.endswith('SHA256SUMS'))] = b'corrupt checksum file'
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, bad)):
        try: package.check(receipt['release_tag'], receipt['source_commit'], 'linux', root/'checksum-mismatch')
        except ValueError: pass
        else: raise SystemExit('corrupt SHA256SUMS was accepted')
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, contents)), patch.object(package, 'run') as gh:
        assert package.publish(pkgdir)['reused']
        assert not any(call.args[:3] == ('gh', 'release', 'upload') for call in gh.call_args_list)
    altered = dict(contents); altered[next(iter(altered))] = b'wrong'
    with patch.object(package, 'release_assets', return_value=records), patch.object(package, 'download_assets', side_effect=mocked_download(records, altered)):
        try: package.publish(pkgdir)
        except ValueError: pass
        else: raise SystemExit('publish would overwrite different bytes')
    mutable = dict(partial)
    def upload_and_list(*args, **kwargs):
        if args[:3] == ('gh', 'release', 'upload'):
            for name,path in files.items(): mutable[name] = {'name':name,'size':path.stat().st_size}
        return b''
    with patch.object(package, 'release_assets', side_effect=lambda tag: mutable), patch.object(package, 'download_assets', side_effect=mocked_download(mutable, contents)) as downloads, patch.object(package, 'run', side_effect=upload_and_list) as gh:
        package.publish(pkgdir)
        upload = next(call.args for call in gh.call_args_list if call.args[:3] == ('gh', 'release', 'upload'))
        assert upload[:4] == ('gh', 'release', 'upload', receipt['release_tag']) and '--clobber' not in upload
        assert {pathlib.Path(name).name for name in upload[6:]} == set(files) - set(partial)
        assert downloads.call_args_list[-1].args[1] == package.platform_asset_names(receipt['release_tag'], 'linux')
PY
  exit 0
fi

[[ $# == 5 && $1 == build ]] || { echo 'usage: custom-release.sh build darwin|linux TAG SHA ABS_OUTPUT' >&2; exit 2; }
platform=$2 tag=$3 sha=$4 out=$5
[[ $platform == darwin || $platform == linux ]] && [[ $out == /* ]] || exit 2
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*$ && $sha =~ ^[0-9a-f]{40}$ ]] || exit 2
[[ $(git rev-parse HEAD) == "$sha" ]] || { echo 'source SHA differs from checkout' >&2; exit 1; }
[[ $(go version) == "go version go1.26.8 $platform/arm64" ]] || { echo 'requires native Go 1.26.8 ARM64' >&2; exit 1; }
[[ ! -e $out ]] || { echo 'output already exists' >&2; exit 1; }
export GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOOS=$platform GOARCH=arm64
export BV_NO_BROWSER=1 BV_TEST_MODE=1 BV_NO_SAVED_CONFIG=1 BV_NO_UPDATE_CHECK=1
mkdir -p "$out/bin" "$out/share/bv"
go build -buildvcs=true -trimpath -mod=vendor \
  -ldflags="-s -w -X github.com/Dicklesworthstone/beads_viewer/pkg/version.version=$tag" \
  -o "$out/bin/bv" ./cmd/bv
[[ $("$out/bin/bv" --version) == "bv $tag" ]]
go version -m "$out/bin/bv" > "$out/build-metadata.txt"
python3 - "$out" "$sha" "$platform" <<'PY'
import json, pathlib, struct, sys
root = pathlib.Path(sys.argv[1])
metadata = (root / 'build-metadata.txt').read_text()
for expected in ('go1.26.8', 'vcs.revision=' + sys.argv[2], 'vcs.modified=false', 'GOOS=' + sys.argv[3], 'GOARCH=arm64', 'CGO_ENABLED=0'):
    if expected not in metadata:
        raise SystemExit('missing Go build metadata: ' + expected)
data = (root / 'bin/bv').read_bytes()[:64]
if sys.argv[3] == 'linux':
    if data[:4] != b'\x7fELF' or data[4:6] != b'\x02\x01' or struct.unpack_from('<H', data, 18)[0] != 183:
        raise SystemExit('not ARM64 ELF')
else:
    if data[:4] != b'\xcf\xfa\xed\xfe' or struct.unpack_from('<I', data, 4)[0] != 0x0100000c:
        raise SystemExit('not ARM64 Mach-O')
(root / 'toolchains.json').write_text(json.dumps({'go':'go1.26.8'}))
PY
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/work/.beads" "$fixture/home" "$fixture/config"
cat > "$fixture/work/.beads/issues.jsonl" <<'EOF'
{"id":"SMOKE-1","title":"Root task","status":"open","priority":1,"issue_type":"task","labels":["core"],"created_at":"2026-10-05T00:00:00Z","updated_at":"2026-10-05T00:00:00Z"}
{"id":"SMOKE-2","title":"Blocked by root","status":"open","priority":2,"issue_type":"task","labels":["core"],"created_at":"2026-10-05T00:00:00Z","updated_at":"2026-10-05T00:00:00Z","dependencies":[{"issue_id":"SMOKE-2","depends_on_id":"SMOKE-1","type":"blocks"}]}
EOF
(
  cd "$fixture/work"
  git init -q
  git add .beads/issues.jsonl
  git -c user.name=smoke -c user.email=smoke@example.invalid commit -qm 'isolated smoke fixture'
  HOME="$fixture/home" XDG_CONFIG_HOME="$fixture/config" "$out/bin/bv" --robot-triage > "$fixture/triage.json"
)
python3 - "$fixture/triage.json" <<'PY'
import json, sys
triage = json.load(open(sys.argv[1]))['triage']
picks = triage['quick_ref']['top_picks']
if not picks or picks[0]['id'] != 'SMOKE-1':
    raise SystemExit('root must be recommended first')
root = next((r for r in triage['recommendations'] if r['id'] == 'SMOKE-1'), None)
if not root or 'SMOKE-2' not in root.get('unblocks_ids', []):
    raise SystemExit('root must unblock SMOKE-2')
PY
cp LICENSE README.md "$out/share/bv/"
while IFS= read -r notice; do cp "$notice" "$out/share/bv/"; done < <(find . -maxdepth 1 -type f -name 'NOTICE*')
mv "$out/build-metadata.txt" "$out/share/bv/build-metadata.txt"
cat > "$out/install.sh" <<'INSTALL'
#!/bin/sh
set -eu
prefix=${HOME:?}/.local
if [ "$#" -gt 0 ]; then
  [ "$#" -eq 2 ] && [ "$1" = --prefix ] || { echo 'usage: install.sh [--prefix ABSOLUTE_DIRECTORY]' >&2; exit 2; }
  prefix=$2
fi
case "$prefix" in /*) ;; *) echo 'prefix must be absolute' >&2; exit 2;; esac
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# Preflight every destination before copying: never overwrite unknown content or follow links.
for dir in "$prefix" "$prefix/bin" "$prefix/share" "$prefix/share/bv"; do
  parent=$dir
  while [ "$parent" != / ]; do
    [ ! -L "$parent" ] || { echo "refusing symlink: $parent" >&2; exit 1; }
    parent=$(dirname -- "$parent")
  done
  [ ! -e "$dir" ] || [ -d "$dir" ] || { echo "not a directory: $dir" >&2; exit 1; }
done
for source in "$root/bin/bv" "$root"/share/bv/*; do
  case "$source" in "$root/bin/bv") dest=$prefix/bin/bv;; *) dest=$prefix/share/bv/$(basename -- "$source");; esac
  [ ! -L "$dest" ] || { echo "refusing symlink: $dest" >&2; exit 1; }
  if [ -e "$dest" ]; then
    [ -f "$dest" ] && cmp -s "$source" "$dest" || { echo "refusing different existing file: $dest" >&2; exit 1; }
  fi
done
mkdir -p "$prefix/bin" "$prefix/share/bv"
for source in "$root/bin/bv" "$root"/share/bv/*; do
  case "$source" in "$root/bin/bv") dest=$prefix/bin/bv;; *) dest=$prefix/share/bv/$(basename -- "$source");; esac
  [ -e "$dest" ] || cp -p "$source" "$dest"
done
INSTALL
chmod +x "$out/install.sh"
"$out/install.sh" --prefix "$fixture/install"
"$out/install.sh" --prefix "$fixture/install"
[[ $(HOME="$fixture/home" "$fixture/install/bin/bv" --version) == "bv $tag" ]]
