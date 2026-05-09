#!/usr/bin/env python3
"""
SDK demo: code-disk attach with version switching in a live sandbox.

Creates a single sandboxd sandbox with TWO code disks:
  /data/v1  ← node-word-count v1 (forest/fox text)
  /data/v2  ← node-word-count v2 (language/words text)

Then uses the toolbox to exec node against each disk — no reboot,
no new VM — demonstrating on-the-fly code switching inside a running
microVM.

Usage:
    # 1. Start sandboxd (in another terminal):
    #    sudo bin/sandboxd -kernel-path artifacts/kernels/gocracker-guest-standard-vmlinux
    #
    # 2. Build the code disks:
    #    bash examples/code-disk/node-word-count/build.sh
    #    bash examples/code-disk/node-word-count/build-v2.sh
    #
    # 3. Run this script:
    #    python3 examples/code-disk/sdk-demo/demo.py

import os
import sys
import time

# Allow running from repo root without installing the SDK.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "../../.."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "../../../sandboxes/sdk/python"))

import gocracker

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../.."))
KERNEL    = os.environ.get("GC_KERNEL", os.path.join(REPO_ROOT, "artifacts/kernels/gocracker-guest-standard-vmlinux"))
SANDBOXD  = os.environ.get("SANDBOXD_URL", "http://127.0.0.1:9090")

DISK_V1 = os.path.join(REPO_ROOT, "examples/code-disk/node-word-count/node-word-count.ext4")
DISK_V2 = os.path.join(REPO_ROOT, "examples/code-disk/node-word-count/node-word-count-v2.ext4")

def check_prereqs():
    missing = []
    if not os.path.isfile(KERNEL):
        missing.append(f"kernel not found: {KERNEL}\n  → run: make kernel-unpack")
    if not os.path.isfile(DISK_V1):
        missing.append(f"v1 disk not found: {DISK_V1}\n  → run: bash examples/code-disk/node-word-count/build.sh")
    if not os.path.isfile(DISK_V2):
        missing.append(f"v2 disk not found: {DISK_V2}\n  → run: bash examples/code-disk/node-word-count/build-v2.sh")
    if missing:
        for m in missing:
            print(f"  prereq missing: {m}", file=sys.stderr)
        sys.exit(2)

def main():
    check_prereqs()

    client = gocracker.Client(base_url=SANDBOXD)

    print("=== gocracker code-disk SDK demo ===\n")
    print(f"sandboxd:  {SANDBOXD}")
    print(f"kernel:    {KERNEL}")
    print(f"disk v1:   {DISK_V1}")
    print(f"disk v2:   {DISK_V2}")
    print()

    # ----------------------------------------------------------------
    # Create one sandbox with BOTH code disks attached simultaneously.
    # v1 mounts at /data/v1, v2 mounts at /data/v2.
    # The VM runs node:20-alpine and stays alive (no --cmd that exits).
    # ----------------------------------------------------------------
    print("[ 1/4 ] Creating sandbox with two code disks...", flush=True)
    t0 = time.monotonic()
    sb = client.create_sandbox(
        image="node:20-alpine",
        kernel_path=KERNEL,
        network_mode="none",
        jailer_mode="off",
        code_disks=[
            {"host_path": DISK_V1, "mount": "/data/v1", "fs_type": "ext4", "read_only": True},
            {"host_path": DISK_V2, "mount": "/data/v2", "fs_type": "ext4", "read_only": True},
        ],
    )
    elapsed = (time.monotonic() - t0) * 1000
    print(f"    sandbox id:    {sb.id}")
    print(f"    state:         {sb.state}")
    print(f"    boot time:     {elapsed:.0f} ms\n")

    tb = sb.toolbox()

    # ----------------------------------------------------------------
    # Exec v1 — reads /data/v1/text.txt (forest/fox content)
    # ----------------------------------------------------------------
    print("[ 2/4 ] Exec v1 (forest/fox text)...", flush=True)
    t1 = time.monotonic()
    r1 = tb.exec(["node", "/data/v1/word-count.js"])
    e1 = (time.monotonic() - t1) * 1000
    import json
    data1 = json.loads(r1.stdout)
    print(f"    exec time:     {e1:.0f} ms")
    print(f"    total_words:   {data1['total_words']}")
    print(f"    unique_words:  {data1['unique_words']}")
    print(f"    top3:          {', '.join(w['word'] + ':' + str(w['count']) for w in data1['top10'][:3])}\n")

    # ----------------------------------------------------------------
    # Exec v2 — reads /data/v2/text.txt (language/words content)
    # Same VM, same node process, different disk path.
    # ----------------------------------------------------------------
    print("[ 3/4 ] Exec v2 (language/words text) — same running VM...", flush=True)
    t2 = time.monotonic()
    r2 = tb.exec(["node", "/data/v2/word-count.js"])
    e2 = (time.monotonic() - t2) * 1000
    data2 = json.loads(r2.stdout)
    print(f"    exec time:     {e2:.0f} ms")
    print(f"    total_words:   {data2['total_words']}")
    print(f"    unique_words:  {data2['unique_words']}")
    print(f"    top3:          {', '.join(w['word'] + ':' + str(w['count']) for w in data2['top10'][:3])}\n")

    # Confirm the top words are different (proves different disk content)
    top_v1 = {w["word"] for w in data1["top10"][:3]}
    top_v2 = {w["word"] for w in data2["top10"][:3]}
    assert top_v1 != top_v2, "top words should differ between v1 and v2"

    # ----------------------------------------------------------------
    # Clean up
    # ----------------------------------------------------------------
    print("[ 4/4 ] Deleting sandbox...", flush=True)
    sb.delete()
    print("    done.\n")

    print("━" * 52)
    print(f"  boot:      {elapsed:.0f} ms  (single VM, two disks)")
    print(f"  exec v1:   {e1:.0f} ms  (top word: {data1['top10'][0]['word']})")
    print(f"  exec v2:   {e2:.0f} ms  (top word: {data2['top10'][0]['word']})")
    print(f"  disk swap: no reboot, no new VM — just a different path")
    print("━" * 52)

if __name__ == "__main__":
    main()
