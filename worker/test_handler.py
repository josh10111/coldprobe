import importlib.util
import sys
import time
import types
import unittest
from unittest import mock

import handler


def test_sleeps_requested_ms():
    t = time.monotonic()
    out = handler.handler({"input": {"sleep_ms": 50}})
    assert out["slept_ms"] == 50 and time.monotonic() - t >= 0.05


def test_rejects_bad_input():
    out = handler.handler({"input": {"sleep_ms": -1}})
    assert "error" in out


def test_caps_sleep():
    assert "error" in handler.handler({"input": {"sleep_ms": 60_001}})


def test_validation_edges():
    for job in ({"input": {"sleep_ms": True}}, {"input": {"sleep_ms": 1.5}}, {"input": {"sleep_ms": "5"}},
                {"input": {}}, {"input": None}, {"input": "x"}, {}):
        assert "error" in handler.handler(job), job
    with mock.patch("handler.time.sleep"):
        for ms in (0, 60_000):
            assert handler.handler({"input": {"sleep_ms": ms}})["slept_ms"] == ms


def test_output_unchanged_without_matmul():
    # items(), not ==: key order must match the pre-matmul_n output too.
    with mock.patch.object(handler, "DEVICE", None), mock.patch("handler.time.sleep"):
        for inp in ({"sleep_ms": 5}, {"sleep_ms": 5, "matmul_n": 0}):
            out = handler.handler({"input": inp})
            assert list(out.items()) == [("slept_ms", 5), ("booted_at", handler.BOOTED_AT)], inp


def test_matmul_validation():
    # No GPU: a bad matmul_n must get the range error, not "needs the GPU image".
    with mock.patch.object(handler, "DEVICE", None):
        for n in (True, -1, 8193, 1.5, "64", None):
            out = handler.handler({"input": {"sleep_ms": 0, "matmul_n": n}})
            assert "matmul_n must be" in out.get("error", ""), n


def test_matmul_failure_still_frees_vram():
    fake = mock.Mock()
    with mock.patch.object(handler, "DEVICE", "fake GPU"), mock.patch.object(handler, "torch", fake), \
            mock.patch.object(handler, "matmul", side_effect=RuntimeError("CUDA out of memory")):
        out = handler.handler({"input": {"sleep_ms": 0, "matmul_n": 64}})
    assert out == {"error": "matmul_n 64 failed: RuntimeError: CUDA out of memory"}
    fake.cuda.empty_cache.assert_called_once()


def test_matmul_without_gpu_errors():
    # DEVICE None with no init error means torch is missing, as in the synthetic image.
    with mock.patch.object(handler, "DEVICE", None):
        out = handler.handler({"input": {"sleep_ms": 0, "matmul_n": 64}})
    assert out == {"error": "matmul_n needs the GPU image (torch with CUDA)"}


def load_with_torch(fake):
    # A fresh copy of the module, so its import-time code runs against the fake torch.
    spec = importlib.util.spec_from_file_location("handler_fake_torch", handler.__file__)
    h = importlib.util.module_from_spec(spec)
    with mock.patch.dict(sys.modules, {"torch": fake}):
        spec.loader.exec_module(h)
    return h


def test_cuda_init_failure_doesnt_crash():
    def broken(*args, **kwargs):
        raise RuntimeError("no kernel image is available")
    # Init raises (no kernels for this GPU arch), or torch finds no usable GPU (driver too old).
    cases = ((True, "no kernel image is available"), (False, "is_available() is False"))
    for available, reason in cases:
        fake = types.SimpleNamespace(cuda=types.SimpleNamespace(is_available=lambda: available),
                                     ones=broken, float16=None)
        h = load_with_torch(fake)
        assert set(h.handler({"input": {"sleep_ms": 0}})) == {"slept_ms", "booted_at"}
        err = h.handler({"input": {"sleep_ms": 0, "matmul_n": 64}})["error"]
        assert "CUDA failed to initialise" in err and reason in err, err


def test_matmul_on_gpu():
    # unittest.SkipTest, not pytest.mark.skipif, so the plain runner below needs no pytest.
    if handler.DEVICE is None:
        raise unittest.SkipTest("needs torch with CUDA")
    out = handler.handler({"input": {"sleep_ms": 0, "matmul_n": 1024}})
    assert out["matmul_ms"] > 0 and out["tflops"] > 0 and out["cuda_init_ms"] > 0 and out["device"]


if __name__ == "__main__":
    tests = (test_sleeps_requested_ms, test_rejects_bad_input, test_caps_sleep, test_validation_edges,
             test_output_unchanged_without_matmul, test_matmul_validation, test_matmul_without_gpu_errors,
             test_matmul_failure_still_frees_vram, test_cuda_init_failure_doesnt_crash, test_matmul_on_gpu)
    for test in tests:
        try:
            test()
        except unittest.SkipTest as e:
            print(f"skipped {test.__name__}: {e}")
    print(f"{len(tests)} run")
