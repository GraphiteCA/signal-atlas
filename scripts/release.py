"""Builds Signal Atlas's release files on this computer, and optionally publishes them.

    python scripts/release.py                    build everything this computer can build
    python scripts/release.py --only linux       just some platforms (windows, linux, macos)
    python scripts/release.py --upload           build, then publish them as a GitHub release

Nothing runs on GitHub's servers: the builds happen here, and --upload only stores the finished
files on the repository's Releases page.

- Windows and Linux are built in a Docker container (packaging/linux/Dockerfile), so any PC with
  Docker builds the same files: a zip with signal-atlas.exe, a Linux tar.gz and a .deb.
- macOS can only be built on a Mac (Apple's frameworks and linker): the script builds a universal
  Signal Atlas.app there (packaging/macos/build.sh), and says it was skipped elsewhere.

The files go to dist/<version>/ with SHA256SUMS.txt. The version comes from the VERSION file
unless --version is given.

Code signing is optional and runs on this computer:
- Windows: set SIGNAL_ATLAS_SIGN_COMMAND to a command that signs one file, with {file} where its
  path goes. signal-atlas.exe is signed before it is zipped and packed into the installer, then the
  installer is signed. For example, with a certificate on a USB token:
    signtool sign /fd sha256 /tr http://timestamp.digicert.com /td sha256 /a {file}
  or with Azure Trusted Signing:
    signtool sign /fd sha256 /tr http://timestamp.acs.microsoft.com /td sha256 /dlib <Azure.CodeSigning.Dlib.dll> /dmdf metadata.json {file}
- macOS (on the Mac): SIGNAL_ATLAS_MAC_IDENTITY="Developer ID Application: Name (TEAMID)" signs
  the app, and SIGNAL_ATLAS_MAC_NOTARY_PROFILE=<a notarytool keychain profile> also notarizes it.
"""
import argparse
import hashlib
import os
import platform
import re
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
IMAGE = "signal-atlas-build:go{go}"


def run(cmd, **kw):
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, **kw)


def project_url():
    src = open(os.path.join(ROOT, "internal", "engine", "tiles.go"), encoding="utf-8").read()
    return re.search(r'ProjectURL\s*=\s*"([^"]+)"', src).group(1)


def go_version():
    src = open(os.path.join(ROOT, "go.mod"), encoding="utf-8").read()
    return re.search(r"^go\s+(\S+)", src, re.M).group(1)


def sign(path):
    """Signs one file with SIGNAL_ATLAS_SIGN_COMMAND, if it is set."""
    template = os.environ.get("SIGNAL_ATLAS_SIGN_COMMAND", "").strip()
    if not template:
        return False
    cmd = template.replace("{file}", '"' + os.path.abspath(path) + '"')
    print("+ sign", os.path.basename(path), flush=True)
    subprocess.run(cmd, shell=True, check=True)
    return True


def build_in_docker(version, out, targets):
    """Builds in the container: the Windows program first (so it can be signed here), then its zip
    and installer, and Linux. The installer is signed last."""
    if not shutil.which("docker"):
        sys.exit("Docker is needed to build for Windows and Linux. Install Docker Desktop, or use --only macos on a Mac.")
    go = go_version()
    image = IMAGE.format(go=go)
    run(["docker", "build", "--build-arg", "GO_VERSION=" + go, "-t", image, os.path.join(ROOT, "packaging", "linux")])
    def container(stage_targets):
        cmd = ["docker", "run", "--rm",
               "-v", ROOT + ":/src:ro", "-v", os.path.abspath(out) + ":/out",
               "-v", "signal-atlas-gomod:/root/go/pkg/mod", "-v", "signal-atlas-gocache:/root/.cache/go-build",
               "-e", "VERSION=" + version, "-e", "TARGETS=" + " ".join(stage_targets), "-e", "PROJECT_URL=" + project_url()]
        if hasattr(os, "getuid"):
            cmd += ["-e", "OWNER=%d:%d" % (os.getuid(), os.getgid())]
        cmd += [image, "sh", "/src/packaging/linux/build-in-container.sh"]
        run(cmd)

    stage = os.path.join(out, ".stage")
    first = (["windows-exe"] if "windows" in targets else []) + (["linux"] if "linux" in targets else [])
    container(first)
    if "windows" in targets:
        signed = sign(os.path.join(stage, "signal-atlas.exe"))
        container(["windows-pack"])
        setup = "signal-atlas-%s-windows-amd64-setup.exe" % version
        sign(os.path.join(stage, setup))
        os.replace(os.path.join(stage, setup), os.path.join(out, setup))
        if not signed:
            print("Not code-signed (set SIGNAL_ATLAS_SIGN_COMMAND to sign): Windows SmartScreen will warn on first run.")
    shutil.rmtree(stage, ignore_errors=True)


def build_macos(version, out):
    run(["sh", os.path.join(ROOT, "packaging", "macos", "build.sh")],
        env=dict(os.environ, VERSION=version, OUT=os.path.abspath(out)), cwd=ROOT)


def checksums(out):
    lines = []
    for name in sorted(os.listdir(out)):
        p = os.path.join(out, name)
        if name == "SHA256SUMS.txt" or not os.path.isfile(p):
            continue
        h = hashlib.sha256()
        with open(p, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                h.update(block)
        lines.append("%s  %s\n" % (h.hexdigest(), name))
    with open(os.path.join(out, "SHA256SUMS.txt"), "w", newline="\n") as f:
        f.writelines(lines)


def upload(version, out, repo):
    if not shutil.which("gh"):
        sys.exit("--upload needs the GitHub CLI (gh), signed in.")
    tag = "v" + version
    files = [os.path.join(out, n) for n in sorted(os.listdir(out)) if os.path.isfile(os.path.join(out, n))]
    exists = subprocess.run(["gh", "release", "view", tag, "--repo", repo], capture_output=True).returncode == 0
    if exists:
        run(["gh", "release", "upload", tag, "--repo", repo, "--clobber"] + files)
    else:
        names = [os.path.basename(f) for f in files]
        lines = ["Signal Atlas %s." % version, ""]
        for suffix, label in (("-windows-amd64-setup.exe", "Windows 10 and 11, installer (updates itself)"),
                              ("-windows-amd64.zip", "Windows 10 and 11, portable zip"),
                              ("-macos-universal.zip", "macOS 12 and newer, Apple silicon and Intel"),
                              ("_amd64.deb", "Ubuntu 22.04+ and Debian 12+"),
                              ("-linux-amd64.tar.gz", "other Linux with GTK 3 and WebKitGTK 4.1")):
            for n in names:
                if n.endswith(suffix):
                    lines.append("- **%s:** `%s`" % (label, n))
        if not any(n.endswith("-macos-universal.zip") for n in names):
            lines.append("- **macOS:** not in this release yet.")
        lines += ["", "The builds aren't code-signed; the README says how to get past the first-run warning. "
                  "SHA256SUMS.txt lists the checksums."]
        notes = "\n".join(lines)
        run(["gh", "release", "create", tag, "--repo", repo, "--title", "Signal Atlas " + version, "--notes", notes] + files)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--version", help="release version (default: the VERSION file)")
    ap.add_argument("--only", help="comma-separated platforms: windows, linux, macos")
    ap.add_argument("--upload", action="store_true", help="publish the files as a GitHub release (needs gh)")
    ap.add_argument("--repo", help="owner/name for --upload (default: from ProjectURL in internal/engine/tiles.go)")
    a = ap.parse_args()
    version = a.version or open(os.path.join(ROOT, "VERSION"), encoding="utf-8").read().strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?", version):
        sys.exit("the version must look like 1.2.3 or 1.2.3-beta.1, not %r" % version)
    on_mac = platform.system() == "Darwin"
    wanted = set((a.only or "windows,linux,macos").split(","))
    unknown = wanted - {"windows", "linux", "macos"}
    if unknown:
        sys.exit("unknown platform: " + ", ".join(sorted(unknown)))
    out = os.path.join(ROOT, "dist", version)
    os.makedirs(out, exist_ok=True)

    docker_targets = [t for t in ("windows", "linux") if t in wanted]
    if docker_targets:
        build_in_docker(version, out, docker_targets)
    skipped = []
    if "macos" in wanted:
        if on_mac:
            build_macos(version, out)
        else:
            skipped.append("macOS (it builds only on a Mac: run this script there)")
    checksums(out)

    print("\nRelease files in", out)
    for n in sorted(os.listdir(out)):
        print("  %-48s %8.1f MB" % (n, os.path.getsize(os.path.join(out, n)) / 1e6))
    for s in skipped:
        print("Skipped:", s)
    if a.upload:
        if skipped:
            print("Uploading without the skipped platforms; run --upload again on a Mac to add the macOS app.")
        upload(version, out, a.repo or re.sub(r"^https://github\.com/", "", project_url()).strip("/"))


if __name__ == "__main__":
    main()
