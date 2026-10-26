#!/usr/bin/env python3
"""Lock same-run tested appliance artifacts for CI-only app-path acceptance."""
import argparse
import json
import os
from pathlib import Path

from components import APPLIANCES, RUNTIMES, PUBLISHER, digest, semver, validate_lock
from release import appliance_contract, verify_assets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    version = semver(Path("APPLIANCE_VERSION").read_text().strip())
    names = APPLIANCES | RUNTIMES
    verify_assets(args.artifacts, names)
    appliance_contract(args.artifacts, version)
    source = os.environ["GITHUB_SHA"]
    descriptor = {"schema": 1, "component": "appliance", "version": version,
                  "source": source, "protocol": 1,
                  "files": {n: digest(args.artifacts / n) for n in sorted(names)}}
    dependency = {"tag": "appliance-v" + version, "release_id": 0, "descriptor": descriptor}
    lock = {"schema": 1, "publisher": PUBLISHER,
            "candidate": {"source": source, "run_id": int(os.environ["GITHUB_RUN_ID"])},
            "appliance": dependency, "runtime": dependency}
    validate_lock(lock, allow_candidate=True)
    args.output.write_text(json.dumps(lock, indent=2) + "\n")


if __name__ == "__main__":
    main()
