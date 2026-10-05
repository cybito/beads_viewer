#!/usr/bin/env python3
"""bv install-package OCI contract; no native-artifact compatibility shim."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile

PROJECT = "beads_viewer"
SOURCE = "https://github.com/cybito/beads_viewer.git"
PACKAGE = "git.cybit.top/cybit/ias-bv"
TYPE = "application/vnd.cybito.install-package.v1"
TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
MEDIA = {"release.json": "application/json", "SHA256SUMS": "text/plain"}


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
    if not TAG.fullmatch(tag) or not SHA.fullmatch(commit) or platform not in ('darwin','linux'):
        raise ValueError("invalid release identity")
    return dict(schema=1, project=PROJECT, source_repo=SOURCE, source_commit=commit,
                release_tag=tag, platform=platform, architecture="arm64")


def hash_bytes(data):
    return hashlib.sha256(data).hexdigest()


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
        data = (directory / name).read_bytes()
        if f["size"] != len(data) or f["sha256"] != hash_bytes(data):
            raise ValueError("payload checksum mismatch")
    sums = "".join(f"{hash_bytes((directory / n).read_bytes())}  {n}\n" for n in sorted(names - {"SHA256SUMS"}))
    if (directory / "SHA256SUMS").read_text() != sums:
        raise ValueError("SHA256SUMS mismatch")
    if {p.name for p in directory.iterdir()} != names or any(not p.is_file() or p.is_symlink() for p in directory.iterdir()):
        raise ValueError("unexpected package files")
    return receipt


def fetch(reference, config):
    p = subprocess.run(["oras", "manifest", "fetch", "--registry-config", str(config), reference], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode:
        error = p.stderr.decode(errors="replace")
        # Only structured distribution error codes mean absent. HTTP/auth/network errors do not.
        if re.search(r"\b(?:MANIFEST_UNKNOWN|NAME_UNKNOWN|manifest_unknown|name_unknown)\b", error):
            return None
        raise RuntimeError(error.strip())
    return p.stdout


def verify(reference, output, config):
    if not re.fullmatch(re.escape(PACKAGE) + r"@sha256:[0-9a-f]{64}", reference):
        raise ValueError("verification requires this project's immutable digest reference")
    raw = fetch(reference, config)
    if raw is None or hash_bytes(raw) != reference.rsplit(":", 1)[1]:
        raise ValueError("manifest digest mismatch")
    manifest = json.loads(raw)
    if manifest.get("artifactType") != TYPE or manifest.get("schemaVersion") != 2:
        raise ValueError("wrong artifact type")
    output.mkdir(parents=True, exist_ok=True)
    if list(output.iterdir()):
        raise ValueError("verification output must be empty")
    names = set()
    with tempfile.TemporaryDirectory() as tmp:
        for descriptor in [manifest["config"], *manifest["layers"]]:
            digest = descriptor["digest"]
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise ValueError("invalid descriptor digest")
            blob = Path(tmp) / digest.split(":")[1]
            run("oras", "blob", "fetch", "--registry-config", str(config), "--output", str(blob), PACKAGE + "@" + digest)
            data = blob.read_bytes()
            if len(data) != descriptor["size"] or hash_bytes(data) != digest.split(":")[1]:
                raise ValueError("descriptor checksum mismatch")
        for layer in manifest["layers"]:
            name = layer.get("annotations", {}).get("org.opencontainers.image.title", "")
            archive_name = re.fullmatch(r"bv-v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*-(darwin|linux)-arm64\.tar\.gz", name)
            if (name not in MEDIA and not archive_name) or name in names or layer["mediaType"] != MEDIA.get(name, "application/gzip"):
                raise ValueError("unsafe or incorrect layer")
            names.add(name)
    run("oras", "pull", "--registry-config", str(config), "--output", str(output), reference)
    receipt = validate(output)
    if names != {p.name for p in output.iterdir()}:
        raise ValueError("layer/payload mismatch")
    for layer in manifest["layers"]:
        data = (output / layer["annotations"]["org.opencontainers.image.title"]).read_bytes()
        if hash_bytes(data) != layer["digest"].split(":")[1] or len(data) != layer["size"]:
            raise ValueError("independent pull mismatch")
    annotations = manifest.get("annotations", {})
    for k, v in {"source": SOURCE, "revision": receipt["source_commit"], "version": receipt["release_tag"]}.items():
        if annotations.get("org.opencontainers.image." + k) != v:
            raise ValueError("source annotation mismatch")
    return receipt


def check(tag, commit, platform, output, config):
    expected = identity(tag, commit, platform)
    reference = f"{PACKAGE}:{tag}-{platform}-arm64"
    raw = fetch(reference, config)
    if raw is None:
        return {"exists": False}
    immutable = PACKAGE + "@sha256:" + hash_bytes(raw)
    receipt = verify(immutable, output, config)
    if any(receipt[k] != v for k, v in expected.items()):
        raise ValueError("published tag belongs to a different release identity")
    return {"exists": True, "reference": immutable}


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
    data = (out / name).read_bytes()
    receipt["files"] = [{"name": name, "sha256": hash_bytes(data), "size": len(data)}]
    (out / "release.json").write_text(json.dumps(receipt, sort_keys=True, indent=2) + "\n")
    (out / "SHA256SUMS").write_text("".join(f"{hash_bytes((out / n).read_bytes())}  {n}\n" for n in sorted([name, "release.json"])))
    validate(out)
    return {"directory": str(out)}


def publish(directory, config):
    receipt = validate(directory)
    with tempfile.TemporaryDirectory() as tmp:
        existing = check(receipt["release_tag"], receipt["source_commit"], receipt["platform"], Path(tmp) / "existing", config)
        if existing["exists"]:
            if json.loads((Path(tmp) / "existing" / "release.json").read_bytes()) != receipt:
                raise ValueError("refusing to replace published bytes")
            return dict(reference=existing["reference"], digest=existing["reference"].split("@", 1)[1])
        created = run("git", "show", "-s", "--format=%cI", receipt["source_commit"]).decode().strip()
        from datetime import datetime, timezone
        created = datetime.fromisoformat(created).astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
        layout = str(Path(tmp) / "layout")
        local = layout + ":release"
        annotations = {"created": created, "source": SOURCE, "revision": receipt["source_commit"], "version": receipt["release_tag"]}
        argv = ["oras", "push", "--oci-layout", local, "--artifact-type", TYPE]
        for k, v in annotations.items():
            argv += ["--annotation", "org.opencontainers.image." + k + "=" + v]
        argv += [p.name + ":" + MEDIA.get(p.name, "application/gzip") for p in sorted(directory.iterdir())]
        run(*argv, cwd=directory)
        raw = run("oras", "manifest", "fetch", "--oci-layout", local)
        digest = "sha256:" + hash_bytes(raw)
        target = f"{PACKAGE}:{receipt['release_tag']}-{receipt['platform']}-arm64"
        late = check(receipt["release_tag"], receipt["source_commit"], receipt["platform"], Path(tmp) / "late", config)
        if late["exists"]:
            old = Path(tmp) / "late"
            if {p.name for p in directory.iterdir()} != {p.name for p in old.iterdir()} or any(p.read_bytes() != (old / p.name).read_bytes() for p in directory.iterdir()):
                raise ValueError("refusing replacement of bytes published during this build")
            return dict(reference=late["reference"], digest=late["reference"].split("@", 1)[1])
        run("oras", "cp", "--from-oci-layout", "--to-registry-config", str(config), local, target)
        reference = PACKAGE + "@" + digest
        verify(reference, Path(tmp) / "pulled", config)
        if fetch(target, config) != raw:
            raise ValueError("published tag manifest mismatch")
        return {"reference": reference, "digest": digest}


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
    sub.add_argument("--registry-config", required=True)
    sub = commands.add_parser("verify")
    sub.add_argument("--reference", required=True)
    sub.add_argument("--output-dir", required=True)
    args = parser.parse_args()
    # Anonymous operations use an explicit isolated config, never host credentials.
    with tempfile.TemporaryDirectory() as tmp:
        config = Path(tmp) / "anonymous.json"
        config.write_text('{"auths":{}}')
        if args.command == "pack":
            result = pack(args)
        elif args.command == "check":
            result = check(args.tag, args.commit, args.platform, absolute(args.output_dir), config)
        elif args.command == "verify":
            result = verify(args.reference, absolute(args.output_dir), config)
        else:
            result = publish(absolute(args.directory), absolute(args.registry_config))
        print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
