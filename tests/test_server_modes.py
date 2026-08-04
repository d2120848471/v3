"""HTTP 快速模式、设备预热容量与无参数启动菜单回归。"""

from __future__ import annotations

import threading
import time
import unittest
from contextlib import contextmanager
from types import SimpleNamespace
from unittest import mock

from ali_slider_reverse.challenge.device_pool import DeviceSessionPool
from ali_slider_reverse.entrypoints import desktop
from ali_slider_reverse.entrypoints.api import SolveRequest, solve_once


class _FakeDeviceClient:
    def __init__(
        self,
        capacity: int,
        *,
        later_cycles_gate: threading.Event | None = None,
    ) -> None:
        self.node_binary = "node"
        self.bridge_script = "bridge.mjs"
        self.sdk_path = None
        self.sdk_url = "https://example.invalid/sdk.js"
        self.prefix = "default"
        self.region = "cn"
        self.timeout = 1.0
        self.gather_cost_range = (180, 260)
        self.first_touch_age_range = (650, 850)
        self.device_profile = SimpleNamespace(profile_id="fixed-profile")
        self.proxies = None
        self.created = 0
        self.closed = 0
        self._lock = threading.Lock()
        self._barrier = threading.Barrier(capacity)
        self._capacity = capacity
        self._later_cycles_gate = later_cycles_gate

    @contextmanager
    def challenge_session(self):
        with self._lock:
            self.created += 1
            sequence = self.created
        # 如果 prime 串行创建，会在这里超时；容量预热必须真正并行。
        self._barrier.wait(timeout=2.0)
        if sequence > self._capacity and self._later_cycles_gate is not None:
            self._later_cycles_gate.wait(timeout=2.0)
        session = SimpleNamespace(
            sequence=sequence,
            process=SimpleNamespace(poll=lambda: None),
        )
        try:
            yield session
        finally:
            with self._lock:
                self.closed += 1

    def wait_until_created(self, expected: int, *, timeout: float = 1.0) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self._lock:
                if self.created >= expected:
                    return True
            time.sleep(0.005)
        return False


class DeviceSessionPoolTests(unittest.TestCase):
    def test_prime_fills_capacity_and_three_leases_use_warm_sessions(self) -> None:
        client = _FakeDeviceClient(capacity=3)
        pool = DeviceSessionPool(enabled=True, capacity=3)
        self.addCleanup(pool.close)

        self.assertEqual(pool.prime(client), 3)
        self.assertEqual(client.created, 3)

        leases = [pool.lease(client) for _ in range(3)]
        runtimes = [lease.__enter__() for lease in leases]
        try:
            sequences = []
            for runtime in runtimes:
                with runtime.challenge_session() as session:
                    sequences.append(session.sequence)
            self.assertEqual(sorted(sequences), [1, 2, 3])
            self.assertEqual(client.created, 3)
        finally:
            for lease in reversed(leases):
                lease.__exit__(None, None, None)

    def test_refill_waits_until_the_active_batch_has_finished(self) -> None:
        client = _FakeDeviceClient(capacity=3)
        pool = DeviceSessionPool(enabled=True, capacity=3)
        self.addCleanup(pool.close)
        self.assertEqual(pool.prime(client), 3)

        leases = [pool.lease(client) for _ in range(3)]
        for lease in leases:
            lease.__enter__()

        leases[0].__exit__(None, None, None)
        self.assertFalse(
            client.wait_until_created(6, timeout=0.1),
            "同批请求尚未结束时不应启动下一批 Node 预热进程",
        )
        leases[1].__exit__(None, None, None)
        self.assertFalse(client.wait_until_created(6, timeout=0.1))

        leases[2].__exit__(None, None, None)
        self.assertTrue(client.wait_until_created(6))

    def test_next_batch_waits_for_in_progress_refill_instead_of_going_cold(
        self,
    ) -> None:
        refill_gate = threading.Event()
        client = _FakeDeviceClient(capacity=3, later_cycles_gate=refill_gate)
        pool = DeviceSessionPool(enabled=True, capacity=3)
        self.addCleanup(pool.close)
        self.assertEqual(pool.prime(client), 3)

        first_batch = [pool.lease(client) for _ in range(3)]
        for lease in first_batch:
            lease.__enter__()
        for lease in first_batch:
            lease.__exit__(None, None, None)
        self.assertTrue(client.wait_until_created(6))

        acquired: list[int] = []

        def consume_one() -> None:
            with (
                pool.lease(client) as runtime,
                runtime.challenge_session() as session,
            ):
                acquired.append(session.sequence)

        workers = [threading.Thread(target=consume_one) for _ in range(3)]
        for worker in workers:
            worker.start()
        try:
            self.assertFalse(
                client.wait_until_created(9, timeout=0.1),
                "补货正在进行时不应再额外冷建一批设备会话",
            )
        finally:
            refill_gate.set()
            for worker in workers:
                worker.join(timeout=2.0)

        self.assertEqual(sorted(acquired), [4, 5, 6])


class SolveOnceProfileTests(unittest.TestCase):
    def test_fixed_profile_is_shared_by_http_and_device_runtime(self) -> None:
        profile = object()
        seen: list[tuple[str, object]] = []
        verify = SimpleNamespace(
            succeeded=True,
            security_token="token",
            verify_code="T001",
            verify_result=True,
            certify_id="certify-id",
        )

        class Client:
            def prewarm_vision(self) -> None:
                return None

            def prewarm_connections(self) -> None:
                return None

            def run_captcha(self, runtime, *, minimum_confidence):
                self.runtime = runtime
                return SimpleNamespace(verify=verify)

            def close(self) -> None:
                return None

        client = Client()

        class Settings:
            minimum_confidence = 0.45

            @staticmethod
            def build_client(*, device_profile, **kwargs):
                seen.append(("http", device_profile))
                return client

            @staticmethod
            def build_device_runtime(*, device_profile, **kwargs):
                seen.append(("device", device_profile))
                return object()

        request = SolveRequest(
            scene_id="scene", proxies=None, rpc_key_id=None, prefix="default"
        )

        result = solve_once(request, Settings(), device_profile=profile)

        self.assertTrue(result["ok"])
        self.assertEqual(seen, [("http", profile), ("device", profile)])


class StartupMenuTests(unittest.TestCase):
    @staticmethod
    def _answers(*values: str):
        answers = iter(values)
        return lambda _prompt="": next(answers)

    def test_enter_selects_recommended_fast_five_concurrency_mode(self) -> None:
        arguments = desktop._select_startup_arguments(input_fn=self._answers(""))

        self.assertEqual(
            arguments,
            ["--runtime-mode", "fast", "--max-concurrency", "5"],
        )

    def test_standard_mode_remains_selectable(self) -> None:
        arguments = desktop._select_startup_arguments(input_fn=self._answers("2"))

        self.assertEqual(
            arguments,
            ["--runtime-mode", "standard", "--max-concurrency", "1"],
        )

    def test_custom_mode_collects_concurrency_port_and_warm_choice(self) -> None:
        arguments = desktop._select_startup_arguments(
            input_fn=self._answers("3", "8", "8010", "n")
        )

        self.assertEqual(
            arguments,
            [
                "--runtime-mode",
                "standard",
                "--max-concurrency",
                "8",
                "--port",
                "8010",
            ],
        )

    def test_no_argument_main_prepares_console_then_dispatches_selection(
        self,
    ) -> None:
        selected = ["--runtime-mode", "fast", "--max-concurrency", "5"]
        with (
            mock.patch.object(desktop, "_prepare_console") as prepare,
            mock.patch.object(
                desktop, "_select_startup_arguments", return_value=selected
            ) as choose,
            mock.patch.object(desktop, "run_console", return_value=17) as run,
        ):
            result = desktop.main([])

        self.assertEqual(result, 17)
        prepare.assert_called_once()
        choose.assert_called_once_with()
        run.assert_called_once_with(selected)


if __name__ == "__main__":
    unittest.main()
