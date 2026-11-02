"""Build source-only Connector releases without copying local configuration."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def run(*args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, text=True, **kwargs).strip()


def build(version):
    if not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?", version):
        raise ValueError("Expected vMAJOR.MINOR.PATCH[-prerelease]")
    output = ROOT / "dist"
    output.mkdir(exist_ok=True)
    run("go", "mod", "download")
    # go list emits concatenated JSON objects, rather than a JSON array.
    modules, remaining = [], run("go", "list", "-m", "-json", "all")
    decoder = json.JSONDecoder()
    while remaining.strip():
        obj, end = decoder.raw_decode(remaining.lstrip())
        modules.append(obj)
        remaining = remaining.lstrip()[end:]
    notices = {}
    for module in modules:
        if module.get("Main"):
            continue
        directory = Path(module["Dir"])
        licenses = [p for p in directory.iterdir() if p.is_file() and p.name.upper().startswith(("LICENSE", "COPYING", "NOTICE", "PATENTS"))]
        if not licenses:
            raise RuntimeError(f"Missing dependency license: {module['Path']}")
        for path in licenses:
            notices[f"licenses/{module['Path']}@{module['Version']}/{path.name}"] = path.read_bytes()
    hashes = []
    for target in ("windows-amd64", "windows-arm64", "darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"):
        system, arch = target.split("-")
        binary_name = "agenttower.exe" if system == "windows" else "agenttower"
        binary = output / (target + (".exe" if system == "windows" else ".bin"))
        env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(binary), "./connector"], cwd=ROOT, env=env, check=True)
        archive = output / f"AgentTower-Connector-{version}-{target}.zip"
        prefix = f"AgentTower-Connector-{version}-{target}/"
        with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as package:
            entry = zipfile.ZipInfo(prefix + binary_name)
            entry.create_system = 3
            entry.external_attr = 0o100755 << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            package.writestr(entry, binary.read_bytes())
            for name in ("LICENSE", "README.md", "README.en.md", "THIRD_PARTY.md"):
                package.write(ROOT / name, prefix + name)
            for name, content in notices.items():
                package.writestr(prefix + name, content)
            package.writestr(prefix + "BUILD_INFO.json", json.dumps({"version": version, "commit": run("git", "rev-parse", "HEAD"), "target": target, "go": run("go", "version")}, indent=2) + "\n")
        binary.unlink()
        hashes.append(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")
        print(archive.name, flush=True)
    (output / "SHA256SUMS.txt").write_text("".join(hashes), encoding="utf-8")


if __name__ == "__main__":
    build(sys.argv[1])
