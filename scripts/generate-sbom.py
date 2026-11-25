#!/usr/bin/env python3
"""Generate a deterministic CycloneDX 1.5 JSON SBOM from Go module metadata (zero dependencies)."""

import argparse
import datetime
import json
import os
import subprocess
import sys
import uuid

DEFAULT_MODULE = "github.com/andreimerlescu/lemmings/v2"


def decode_json_stream(text):
    dec = json.JSONDecoder()
    idx = 0
    objs = []
    length = len(text)
    while idx < length:
        while idx < length and text[idx].isspace():
            idx += 1
        if idx >= length:
            break
        obj, end = dec.raw_decode(text, idx)
        objs.append(obj)
        idx = end
    return objs


def load_go_modules(pkg=None, tags=None):
    if pkg:
        cmd = ["go", "list", "-deps", "-json"]
        if tags:
            cmd.extend(["-tags", tags])
        cmd.append(pkg)
        pkgs = decode_json_stream(subprocess.check_output(cmd, text=True))
        seen = {}
        for p in pkgs:
            m = p.get("Module")
            if not m:
                continue
            path = m.get("Path", "")
            if path and path not in seen:
                seen[path] = m
        return sorted(seen.values(), key=lambda m: (not m.get("Main", False), m.get("Path", "")))
    out = subprocess.check_output(["go", "list", "-m", "-json", "all"], text=True)
    mods = decode_json_stream(out)
    return sorted(mods, key=lambda m: (not m.get("Main", False), m.get("Path", ""), m.get("Version", "")))


def purl(path, version):
    if version:
        return f"pkg:golang/{path}@{version}"
    return f"pkg:golang/{path}"


def deterministic_timestamp():
    sde = os.environ.get("SOURCE_DATE_EPOCH", "").strip()
    if sde:
        try:
            dt = datetime.datetime.fromtimestamp(int(sde), tz=datetime.timezone.utc)
            return dt.strftime("%Y-%m-%dT%H:%M:%SZ")
        except ValueError:
            pass
    try:
        raw = subprocess.check_output(
            ["git", "show", "-s", "--format=%ct", "HEAD"],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
        if raw:
            dt = datetime.datetime.fromtimestamp(int(raw), tz=datetime.timezone.utc)
            return dt.strftime("%Y-%m-%dT%H:%M:%SZ")
    except (OSError, ValueError, subprocess.CalledProcessError):
        pass
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def main():
    parser = argparse.ArgumentParser(description="Generate CycloneDX 1.5 SBOM for Lemmings")
    parser.add_argument("-o", "--output", default="bin/sbom.cdx.json", help="Output SBOM JSON path")
    parser.add_argument("--pkg", default="", help="Optional target package (e.g. ./cmd/lemmings) to emit only linked modules")
    parser.add_argument("--tags", default="", help="Optional Go build tags")
    parser.add_argument("--name", default="", help="Optional component name override (e.g. lemmings)")
    args = parser.parse_args()

    mods = load_go_modules(pkg=args.pkg or None, tags=args.tags or None)
    main_mod = next((m for m in mods if m.get("Main")), {"Path": DEFAULT_MODULE})
    try:
        with open("VERSION", "r", encoding="utf-8") as f:
            app_version = f.read().strip()
    except OSError:
        app_version = "0.0.0-dev"

    comp_path = main_mod.get("Path", DEFAULT_MODULE)
    if args.name:
        comp_name = f"{comp_path}/cmd/{args.name}"
    else:
        comp_name = comp_path

    components = []
    for m in mods:
        if m.get("Main"):
            continue
        path = m.get("Path", "")
        ver = m.get("Version", "")
        if not path:
            continue
        comp = {
            "type": "library",
            "name": path,
            "version": ver,
            "purl": purl(path, ver),
            "bom-ref": purl(path, ver),
            "scope": "optional" if m.get("Indirect") else "required",
        }
        components.append(comp)

    components.sort(key=lambda c: (c["name"], c["version"]))

    serial_seed = f"{comp_name}@{app_version}|" + "|".join(c["purl"] for c in components)
    serial_uuid = uuid.uuid5(uuid.NAMESPACE_URL, serial_seed)

    sbom = {
        "bomFormat": "CycloneDX",
        "specVersion": "1.5",
        "serialNumber": f"urn:uuid:{serial_uuid}",
        "version": 1,
        "metadata": {
            "timestamp": deterministic_timestamp(),
            "component": {
                "type": "application",
                "name": comp_name,
                "version": app_version,
                "licenses": [{"license": {"id": "Apache-2.0"}}],
                "purl": purl(comp_name, app_version),
            },
        },
        "components": components,
    }

    out_dir = os.path.dirname(args.output)
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    with open(args.output, "w", encoding="utf-8") as f:
        json.dump(sbom, f, indent=2)
        f.write("\n")
    print(f"Wrote CycloneDX 1.5 SBOM with {len(components)} components to {args.output}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
