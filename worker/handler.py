"""Runpod Serverless worker: sleeps for the requested time and reports when it booted.

No real work by default, so a benchmark measures only platform overhead. An optional
`matmul_n` adds a timed fp16 matmul on the GPU image (Dockerfile.gpu).
"""

import os
import statistics
import time

import runpod

# Same value on every job this process serves, so the client can spot cold workers.
BOOTED_AT = time.time()

# torch is only installed in the GPU image.
try:
    import torch
except ImportError:
    torch = None

# CUDA init is part of a real cold start, so it is timed once at import (excluding `import torch`).
CUDA_INIT_MS = None
DEVICE = None
CUDA_INIT_ERROR = None
if torch is not None:
    try:
        t0 = time.perf_counter()
        if torch.cuda.is_available():  # starts the driver, so it's inside the timer
            # A tiny matmul rather than torch.cuda.init(), so cuBLAS loading isn't billed to the first job.
            x = torch.ones(8, 8, device="cuda", dtype=torch.float16)
            x @ x
            torch.cuda.synchronize()
            CUDA_INIT_MS = (time.perf_counter() - t0) * 1000
            DEVICE = torch.cuda.get_device_name()
            del x
        else:
            # torch only warns, not raises, when the host driver is too old for its CUDA build.
            CUDA_INIT_ERROR = "torch.cuda.is_available() is False (no GPU, or a driver too old for this CUDA)"
    # Raising here would kill the worker and hang its jobs until timeout; keep serving sleep-only jobs.
    except Exception as e:
        CUDA_INIT_MS = DEVICE = None  # the failure may come after one of them was set
        CUDA_INIT_ERROR = f"{type(e).__name__}: {e}"

# Simulated model load: counts towards the cold start, not each job.
time.sleep(float(os.environ.get("MODEL_LOAD_SECONDS", "0")))

# Caps a typo (60000 vs 600000) from keeping a paid worker busy for minutes.
MAX_SLEEP_MS = 60_000
# Three 8192 fp16 matrices are 128 MB each: fits a small MIG slice, runs in well under a second.
MAX_MATMUL_N = 8192
MATMUL_ITERS = 10


def int_in_range(v, hi: int) -> bool:
    return isinstance(v, int) and not isinstance(v, bool) and 0 <= v <= hi


def matmul(n: int) -> tuple[float, float]:
    """Median ms per n x n fp16 matmul, and the TFLOPS that implies."""
    a = torch.randn(n, n, device="cuda", dtype=torch.float16)
    b = torch.randn(n, n, device="cuda", dtype=torch.float16)
    a @ b  # warm-up: the first call at a new size picks a cuBLAS kernel
    # Events are queued back to back with one sync after the loop, so each interval is just the
    # kernel. Small n lets launch gaps creep in and reads low; use n >= 4096 to compare GPUs.
    starts = [torch.cuda.Event(enable_timing=True) for _ in range(MATMUL_ITERS)]
    ends = [torch.cuda.Event(enable_timing=True) for _ in range(MATMUL_ITERS)]
    for start, end in zip(starts, ends):
        start.record()
        a @ b
        end.record()
    torch.cuda.synchronize()
    times = [start.elapsed_time(end) for start, end in zip(starts, ends)]  # ms
    ms = statistics.median(times)
    return ms, 2 * n**3 / (ms / 1000) / 1e12


def handler(job: dict) -> dict:
    inp = job.get("input")
    if not isinstance(inp, dict):
        return {"error": f"input must be an object, got {inp!r}"}
    sleep_ms = inp.get("sleep_ms")
    if not int_in_range(sleep_ms, MAX_SLEEP_MS):
        return {"error": f"sleep_ms must be an integer between 0 and {MAX_SLEEP_MS}, got {sleep_ms!r}"}
    matmul_n = inp.get("matmul_n", 0)
    if not int_in_range(matmul_n, MAX_MATMUL_N):
        return {"error": f"matmul_n must be an integer between 0 and {MAX_MATMUL_N}, got {matmul_n!r}"}
    # Checked after the range, so matmul_n 0 still works on the synthetic image.
    if matmul_n and CUDA_INIT_ERROR:
        return {"error": f"matmul_n: CUDA failed to initialise on this worker: {CUDA_INIT_ERROR}"}
    if matmul_n and DEVICE is None:
        return {"error": "matmul_n needs the GPU image (torch with CUDA)"}

    out = {}
    if DEVICE is not None:
        out |= {"cuda_init_ms": CUDA_INIT_MS, "device": DEVICE}
    if matmul_n:
        try:
            out["matmul_ms"], out["tflops"] = matmul(matmul_n)
        # Returned, not raised: a raised traceback would keep matmul's tensors alive past empty_cache.
        except Exception as e:
            return {"error": f"matmul_n {matmul_n} failed: {type(e).__name__}: {e}"}
        finally:
            # Release the allocator's cached VRAM so an idle worker doesn't hold it, even after an OOM.
            torch.cuda.empty_cache()
    time.sleep(sleep_ms / 1000)
    return {"slept_ms": sleep_ms, "booted_at": BOOTED_AT} | out


if __name__ == "__main__":
    runpod.serverless.start({"handler": handler})
