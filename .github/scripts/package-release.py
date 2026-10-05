#!/usr/bin/env python3
"""bv install-package GitHub Release asset contract."""
import argparse
import filecmp
import gzip
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

PROJECT = "beads_viewer"
SOURCE = "https://github.com/cybito/beads_viewer.git"
REPOSITORY = "cybito/beads_viewer"
TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
MAX_FILE_SIZE = 2 * 1024 * 1024 * 1024
MAX_RELEASE_ASSETS = 1000


def run(*args, cwd=None):
    p = subprocess.run(args, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode:
        raise RuntimeError(p.stderr.decode(errors="replace").strip())
    return p.stdout


def absolute(value):
    p = Path(value)
    if not p.is_absolute():
        raise ValueError("directory must be absolute")
    return p


def identity(tag, commit, platform):
    if not TAG.fullmatch(tag) or not SHA.fullmatch(commit) or platform not in ('darwin', 'linux'):
        raise ValueError("invalid release identity")
    return dict(schema=1, project=PROJECT, source_repo=SOURCE, source_commit=commit,
                release_tag=tag, platform=platform, architecture="arm64")


def hash_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def file_size(path):
    return path.stat().st_size


def validate(directory):
    receipt = json.loads((directory / "release.json").read_bytes())
    keys = {"schema", "project", "source_repo", "source_commit", "release_tag", "platform", "architecture", "toolchains", "files"}
    if set(receipt) != keys or receipt["platform"] not in ("darwin", "linux"):
        raise ValueError("invalid receipt schema")
    expected = identity(receipt["release_tag"], receipt["source_commit"], receipt["platform"])
    if any(receipt[k] != v for k, v in expected.items()):
        raise ValueError("receipt identity mismatch")
    if receipt["toolchains"] != {"go": "go1.26.8"}:
        raise ValueError("requires verified Go 1.26.8 toolchain")
    names = {"release.json", "SHA256SUMS"}
    if not isinstance(receipt["files"], list) or len(receipt["files"]) != 1:
        raise ValueError("expected one bv installation archive")
    for f in receipt["files"]:
        name = f["name"]
        if set(f) != {"name", "sha256", "size"} or name != f"bv-{receipt['release_tag']}-{receipt['platform']}-arm64.tar.gz" or name in names:
            raise ValueError("invalid payload filename")
        names.add(name)
        path = directory / name
        if f["size"] != file_size(path) or f["sha256"] != hash_file(path):
            raise ValueError("payload checksum mismatch")
    sums = "".join(f"{hash_file(directory / n)}  {n}\n" for n in sorted(names - {"SHA256SUMS"}))
    if (directory / "SHA256SUMS").read_text() != sums:
        raise ValueError("SHA256SUMS mismatch")
    if {p.name for p in directory.iterdir()} != names or any(not p.is_file() or p.is_symlink() for p in directory.iterdir()):
        raise ValueError("unexpected package files")
    return receipt


def asset_name(tag, platform, name):
    return f"{tag}-{platform}-{name}"


def expected_assets(directory, receipt):
    return {asset_name(receipt["release_tag"], receipt["platform"], p.name): p for p in directory.iterdir()}


def release_assets(tag):
    data = json.loads(run("gh", "release", "view", tag, "--repo", REPOSITORY, "--json", "assets"))
    return {item["name"]: item for item in data["assets"]}


def download_assets(tag, assets, output):
    output.mkdir(parents=True, exist_ok=True)
    if list(output.iterdir()):
        raise ValueError("verification output must be empty")
    args = ["gh", "release", "download", tag, "--repo", REPOSITORY, "--dir", str(output)]
    for name in assets:
        args.extend(["--pattern", name])
    run(*args)

def platform_asset_names(tag, platform):
    return [asset_name(tag, platform, name) for name in (
        "release.json", "SHA256SUMS", f"bv-{tag}-{platform}-arm64.tar.gz"
    )]


def reject_unexpected_platform_assets(tag, platform, present, expected_names):
    prefix = f"{tag}-{platform}-"
    unexpected = [name for name in present if name.startswith(prefix) and name not in expected_names]
    if unexpected:
        raise ValueError("unexpected assets for this platform: " + ", ".join(sorted(unexpected)))


def verify_download(tag, commit, platform, output, assets=None):
    expected = identity(tag, commit, platform)
    present = release_assets(tag) if assets is None else assets
    names = platform_asset_names(tag, platform)
    reject_unexpected_platform_assets(tag, platform, present, names)
    relevant = [name for name in names if name in present]
    if not relevant:
        return {"exists": False}
    if len(relevant) != len(names):
        # Existing names are compared against freshly built bytes in publish;
        # this permits safe recovery of an interrupted platform upload.
        return {"exists": False, "partial": True}
    download_assets(tag, relevant, output)
    local = {p.name: p for p in output.iterdir()}
    if set(local) != set(names):
        raise ValueError("downloaded asset set is incomplete or contains unexpected files")
    for name in names:
        if present[name].get("size", local[name].stat().st_size) != local[name].stat().st_size:
            raise ValueError("downloaded asset size differs from release metadata")
    package_dir = output / "package"
    package_dir.mkdir()
    prefix = f"{tag}-{platform}-"
    for name in names:
        (output / name).rename(package_dir / name[len(prefix):])
    receipt = validate(package_dir)
    if any(receipt[k] != v for k, v in expected.items()):
        raise ValueError("published assets belong to a different release identity")
    for name, path in expected_assets(package_dir, receipt).items():
        if present[name].get("size", file_size(path)) != file_size(path):
            raise ValueError("asset metadata size mismatch")
    return {"exists": True, "assets": names, "directory": str(package_dir)}


def check(tag, commit, platform, output):
    return verify_download(tag, commit, platform, output)


def pack(args):
    receipt = identity(args.tag, args.commit, args.platform)
    root, out = absolute(args.input_dir), absolute(args.output_dir)
    if run("git", "rev-parse", "HEAD").decode().strip() != args.commit:
        raise ValueError("source SHA differs from HEAD")
    epoch = int(run("git", "show", "-s", "--format=%ct", args.commit))
    out.mkdir(parents=True, exist_ok=True)
    if list(out.iterdir()):
        raise ValueError("package output must be empty")
    receipt["toolchains"] = json.loads((root / "toolchains.json").read_text())
    name = f"bv-{args.tag}-{args.platform}-arm64.tar.gz"
    with (out / name).open("wb") as stream, gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=epoch) as gz, tarfile.open(fileobj=gz, mode="w") as archive:
        for path in sorted(root.rglob("*")):
            if not path.is_file() or path.name == "toolchains.json":
                continue
            if path.is_symlink():
                raise ValueError("unexpected payload symlink")
            info = archive.gettarinfo(str(path), str(path.relative_to(root)))
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = epoch
            with path.open("rb") as data:
                archive.addfile(info, data)
    archive_path = out / name
    receipt["files"] = [{"name": name, "sha256": hash_file(archive_path), "size": file_size(archive_path)}]
    (out / "release.json").write_text(json.dumps(receipt, sort_keys=True, indent=2) + "\n")
    (out / "SHA256SUMS").write_text("".join(f"{hash_file(out / n)}  {n}\n" for n in sorted([name, "release.json"])))
    validate(out)
    return {"directory": str(out)}


def publish(directory):
    receipt = validate(directory)
    assets = release_assets(receipt["release_tag"])
    files = expected_assets(directory, receipt)
    reject_unexpected_platform_assets(receipt["release_tag"], receipt["platform"], assets, set(files))
    for name, path in files.items():
        if path.stat().st_size >= MAX_FILE_SIZE:
            raise ValueError(f"asset exceeds GitHub's per-file limit: {path.name}")
        if name in assets:
            with tempfile.TemporaryDirectory() as tmp:
                downloaded = Path(tmp)
                download_assets(receipt["release_tag"], [name], downloaded)
                existing = downloaded / name
                if not filecmp.cmp(existing, path, shallow=False):
                    raise ValueError(f"refusing to overwrite mismatching release asset: {name}")
    missing = [(name, path) for name, path in files.items() if name not in assets]
    if not missing:
        return {"assets": sorted(files), "reused": True}
    if len(assets) + len(missing) > MAX_RELEASE_ASSETS:
        raise ValueError("upload would exceed GitHub's 1000 assets per release limit")
    with tempfile.TemporaryDirectory() as tmp:
        upload_dir = Path(tmp)
        upload_paths = []
        for name, path in missing:
            destination = upload_dir / name
            shutil.copyfile(path, destination)
            upload_paths.append(str(destination))
        run("gh", "release", "upload", receipt["release_tag"], "--repo", REPOSITORY, *upload_paths)
    with tempfile.TemporaryDirectory() as tmp:
        result = verify_download(receipt["release_tag"], receipt["source_commit"], receipt["platform"], Path(tmp))
        if not result["exists"]:
            raise ValueError("uploaded platform assets were not readable")
    return {"assets": sorted(files), "reused": False}


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("check", "pack"):
        sub = commands.add_parser(name)
        sub.add_argument("--tag", required=True)
        sub.add_argument("--commit", required=True)
        sub.add_argument("--platform", choices=("darwin", "linux"), required=True)
        sub.add_argument("--output-dir", required=True)
        if name == "pack":
            sub.add_argument("--input-dir", required=True)
    sub = commands.add_parser("publish")
    sub.add_argument("--directory", required=True)
    args = parser.parse_args()
    if args.command == "pack":
        result = pack(args)
    elif args.command == "check":
        result = check(args.tag, args.commit, args.platform, absolute(args.output_dir))
    else:
        result = publish(absolute(args.directory))
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
