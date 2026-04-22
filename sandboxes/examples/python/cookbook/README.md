# gocracker Python cookbook

5 canonical examples per `PLAN_SANDBOXD.md` §8, end-to-end against
a running sandboxd.

## Setup

```bash
# 1. Start sandboxd (must run as root for KVM + jailer):
sudo gocracker-sandboxd serve -addr :9091

# 2. Run any example. Each adds the SDK to sys.path automatically
#    so no `pip install` is needed.
python hello_world.py /abs/path/to/vmlinux
```

## Examples

| File | What |
|---|---|
| `hello_world.py` | create + exec `echo hello` (smoke) |
| `exec_stream.py` | exec with stdout streaming (1 line/s for 5 s) |
| `files.py` | upload + read + list (waits on toolbox files API) |
| `preview.py` | guest server :8080, mint URL, curl from host |
| `pool_burst.py` | 50 concurrent leases against a warm pool, p95 stats |
| `files_full.py` | full toolbox files surface (upload / list / download / mkdir / rename / chmod / delete) |
| `git_clone.py` | clone a public repo inside the guest + git status |
| `secrets.py` | toolbox /secrets set / list / delete |
| `template_pool.py` | template → pool (from_template) → lease pipeline, times each step |
| `concurrent_cold.py` | N concurrent cold-boots (stress test, reproduces artifact_lock correctness) |

## Notes

- `files.py` exits 2 if the toolbox agent doesn't expose the
  files endpoints (current build doesn't; the SDK shape is in
  place for when it does).
- `preview.py` requires `python:3.12-alpine` so the tiny HTTP
  server has Python in the guest. Swap the image if you want
  a different runtime.
- `pool_burst.py` reuses an existing pool with the same
  `template_id` if one is already registered. Default `template_id`
  is `burst-pool`.
