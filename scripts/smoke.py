#!/usr/bin/env python3
"""Exercise the actual CLI, optional Chromium cohort, reports, gates and Ctrl-C.

Run from the repo root: python3 scripts/smoke.py [--browser] [--output DIR]
Only the bundled local demonstration server receives traffic.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--browser", action="store_true")
    parser.add_argument("--output")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    os.chdir(root)
    with tempfile.TemporaryDirectory(prefix="lemmings-smoke-") as tmp:
        work = Path(args.output).resolve() if args.output else Path(tmp) / "results"
        work.mkdir(parents=True, exist_ok=True)
        demo = str(Path(tmp) / "demo")
        binary = str(Path(tmp) / "lemmings")
        subprocess.run(["go", "build", "-o", binary, "."], check=True)
        subprocess.run(["go", "build", "-o", demo, "./examples/demo"], check=True)
        version = subprocess.run([binary, "-version", "-cd", "2"], check=True, capture_output=True, text=True)
        assert version.stdout.strip(), "CLI version/alias setup failed"
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            port = s.getsockname()[1]
        url = f"http://127.0.0.1:{port}/"
        server = subprocess.Popen([demo, "-addr", f"127.0.0.1:{port}"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(100):
                try:
                    urllib.request.urlopen(url, timeout=0.1).close()
                    break
                except OSError:
                    time.sleep(0.025)
            else:
                raise RuntimeError("demo server did not start")
            common = [binary, "-hit", url, "-terrain", "2", "-pack", "3", "-limit", "3", "-until", "5s", "-ramp", "20ms", "-think-min", "20ms", "-think-max", "50ms", "-request-timeout", "500ms", "-tty=false", "-max-failure-rate", "0", "-p95-budget", "2s"]
            summary = {}
            for name, pages, failures in [("browse", 3, 0), ("broken", 4, 18)]:
                dest = work / name
                dest.mkdir(exist_ok=True)
                cmd = common + ["-scenario", f"examples/{name}.json", "-save-to", str(dest), "-trace-file", str(dest / "http-visits.jsonl")]
                if args.browser:
                    cmd += ["-browser-users", "2", "-browser-until", "8s", "-browser-output", str(dest / "browser")]
                with (dest / "console.log").open("w") as log:
                    result = subprocess.run(cmd, stdout=log, stderr=subprocess.STDOUT, timeout=30)
                assert result.returncode == (2 if failures else 0), (name, result.returncode, (dest / "console.log").read_text())
                report_path = next((dest / "lemmings").rglob("*.json"))
                report = json.loads(report_path.read_text())
                x = report["Experience"]
                assert report["TotalVisits"] == pages * 6, report["TotalVisits"]
                assert x["failed_visits"] == failures, x
                assert x["sessions_observed"] == 6
                assert x["trace_dropped"] == x["dropped_lifelogs"] == 0
                assert sum(x["final_status_codes"].values()) == pages * 6
                assert len((dest / "http-visits.jsonl").read_text().splitlines()) == pages * 6
                if args.browser:
                    browser = x["browser"]
                    assert not browser.get("error"), browser
                    assert browser["visits"] == pages * 2, browser
                    assert browser["failed"] == (6 if failures else 0), browser
                summary[name] = {"exit_code": result.returncode, "http_visits": report["TotalVisits"], "http_failures": failures, "browser_visits": x.get("browser", {}).get("visits", 0), "browser_failures": x.get("browser", {}).get("failed", 0)}
            dest = work / "cancelled"
            dest.mkdir(exist_ok=True)
            cmd = common + ["-until", "30s", "-max-pages", "0", "-save-to", str(dest), "-trace-file", str(dest / "http-visits.jsonl")]
            with (dest / "console.log").open("w") as log:
                child = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT)
                try:
                    for _ in range(100):
                        if (dest / "http-visits.jsonl").exists():
                            break
                        time.sleep(0.02)
                    time.sleep(0.15)
                    child.send_signal(signal.SIGINT)
                    code = child.wait(timeout=10)
                finally:
                    if child.poll() is None:
                        child.kill()
                        child.wait()
            assert code == 130, ("cancellation exit", code)
            report = json.loads(next((dest / "lemmings").rglob("*.json")).read_text())
            assert report["TotalVisits"] > 0, "cancelled run lost its evidence"
            summary["cancelled"] = {"exit_code": code, "http_visits": report["TotalVisits"], "report_preserved": True}
            (work / "verification.json").write_text(json.dumps(summary, indent=2) + "\n")
            print(json.dumps(summary, indent=2))
        finally:
            server.terminate()
            server.wait(timeout=5)


if __name__ == "__main__":
    main()
