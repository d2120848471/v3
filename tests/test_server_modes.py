"""HTTP 快速模式、设备预热容量与无参数启动菜单回归。"""

from __future__ import annotations

import io
import json
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request
from contextlib import contextmanager, redirect_stderr
from http.server import ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from ali_slider_reverse import config
from ali_slider_reverse.device_profile import generate_device_profile
from ali_slider_reverse.challenge.device_pool import DeviceSessionPool
from ali_slider_reverse.challenge.session import AliSliderClient
from ali_slider_reverse.challenge.transport import (
    SharedHttpAdapterPool,
    warm_connections,
)
from ali_slider_reverse.entrypoints import api, desktop
from ali_slider_reverse.entrypoints.api import (
    SliderApiHandler,
    SolveRequest,
    solve_once,
)
from ali_slider_reverse.runtime import node_device
from ali_slider_reverse.runtime.node_device import DeviceRuntimeClient


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


class _FakeHostSession:
    def __init__(
        self,
        client: "_FakeGroupedDeviceClient",
        *,
        host_id: int,
        process: object,
        sequence: int,
    ) -> None:
        self.client = client
        self.host_id = host_id
        self.initialized_at = time.monotonic()
        self.process = process
        self.sequence = sequence
        self._closed = False

    @property
    def recyclable(self) -> bool:
        return True

    def recycle(self) -> None:
        with self.client._lock:
            if self._closed:
                raise RuntimeError("closed host session")
            self.client.recycled += 1
            self.initialized_at = time.monotonic()

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()

    def close(self) -> None:
        with self.client._lock:
            if self._closed:
                return
            self._closed = True
            self.client.closed += 1


class _FakeGroupedDeviceClient(_FakeDeviceClient):
    def __init__(self, capacity: int) -> None:
        super().__init__(capacity)
        self.host_groups: list[int] = []
        self.recycled = 0

    def wait_until_recycled(self, expected: int, *, timeout: float = 1.0) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self._lock:
                if self.recycled >= expected:
                    return True
            time.sleep(0.005)
        return False

    def open_challenge_sessions(
        self, count: int
    ) -> tuple[_FakeHostSession, ...]:
        with self._lock:
            host_id = len(self.host_groups) + 1
            self.host_groups.append(count)
            first_sequence = self.created + 1
            self.created += count
        process = SimpleNamespace(poll=lambda: None)
        return tuple(
            _FakeHostSession(
                self,
                host_id=host_id,
                process=process,
                sequence=first_sequence + index,
            )
            for index in range(count)
        )


class DeviceSessionPoolTests(unittest.TestCase):
    def test_multi_vm_pool_groups_five_sessions_into_two_node_hosts(self) -> None:
        client = _FakeGroupedDeviceClient(capacity=5)
        pool = DeviceSessionPool(enabled=True, capacity=5, vms_per_node=3)
        self.addCleanup(pool.close)

        self.assertEqual(pool.prime(client), 5)
        self.assertEqual(sorted(client.host_groups), [2, 3])

        leases = [pool.lease(client) for _ in range(5)]
        runtimes = [lease.__enter__() for lease in leases]
        try:
            host_ids = []
            for runtime in runtimes:
                with runtime.challenge_session() as session:
                    host_ids.append(session.host_id)
            host_sizes = sorted(host_ids.count(host_id) for host_id in set(host_ids))
            self.assertEqual(host_sizes, [2, 3])
            self.assertEqual(client.created, 5)
            # 先关池，避免退出 lease 后触发后台 VM 重建。
            pool.close()
        finally:
            for lease in reversed(leases):
                lease.__exit__(None, None, None)

        self.assertEqual(client.closed, 5)

    def test_multi_vm_recycles_one_slot_before_siblings_finish(self) -> None:
        client = _FakeGroupedDeviceClient(capacity=5)
        pool = DeviceSessionPool(enabled=True, capacity=5, vms_per_node=3)
        self.addCleanup(pool.close)
        self.assertEqual(pool.prime(client), 5)

        leases = [pool.lease(client) for _ in range(5)]
        runtimes = [lease.__enter__() for lease in leases]
        first_host_id = None
        with runtimes[0].challenge_session() as session:
            first_host_id = session.host_id

        leases[0].__exit__(None, None, None)
        self.assertTrue(
            client.wait_until_recycled(1),
            "单个槽位结束后应立即在原 Node host 内重建",
        )

        sixth = pool.lease(client)
        sixth_runtime = sixth.__enter__()
        try:
            with sixth_runtime.challenge_session() as session:
                self.assertEqual(session.host_id, first_host_id)
            self.assertEqual(sorted(client.host_groups), [2, 3])
            self.assertEqual(client.created, 5)
            # 仍有四条首批 lease 在运行，第六条已经取得刚重建的槽位。
            self.assertEqual(pool._leased, 5)
            pool.close()
        finally:
            sixth.__exit__(None, None, None)
            for lease in reversed(leases[1:]):
                lease.__exit__(None, None, None)

        self.assertEqual(client.closed, 5)

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

    def test_one_vm_refill_starts_before_active_batch_has_finished(self) -> None:
        client = _FakeDeviceClient(capacity=3)
        pool = DeviceSessionPool(enabled=True, capacity=3)
        self.addCleanup(pool.close)
        self.assertEqual(pool.prime(client), 3)

        leases = [pool.lease(client) for _ in range(3)]
        for lease in leases:
            lease.__enter__()

        leases[0].__exit__(None, None, None)
        self.assertTrue(
            client.wait_until_created(4),
            "首个槽位结束后就应启动对应补货",
        )
        leases[1].__exit__(None, None, None)
        self.assertTrue(client.wait_until_created(5))

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

    def test_waiting_pool_lease_can_cancel_without_leaking_active_count(
        self,
    ) -> None:
        refill_gate = threading.Event()
        self.addCleanup(refill_gate.set)
        client = _FakeDeviceClient(capacity=1, later_cycles_gate=refill_gate)
        pool = DeviceSessionPool(enabled=True, capacity=1)
        self.addCleanup(pool.close)
        self.assertEqual(pool.prime(client), 1)

        with pool.lease(client):
            pass
        self.assertTrue(client.wait_until_created(2))

        def cancelled() -> None:
            raise RuntimeError("client disconnected")

        with self.assertRaisesRegex(RuntimeError, "client disconnected"):
            with pool.lease(client, cancel_check=cancelled):
                self.fail("取消后不应交出设备会话")

        self.assertEqual(pool._leased, 0)


class DeviceRuntimeHostTests(unittest.TestCase):
    def test_python_host_demultiplexes_two_sessions_over_one_process(self) -> None:
        bridge_source = """
import json
import sys
import time

args = sys.argv[1:]
count = int(args[args.index("--vm-count") + 1])
for session_id in reversed(range(count)):
    print(json.dumps({
        "sessionId": session_id,
        "stage": "init",
        "deviceToken": f"init-{session_id}",
    }), flush=True)
    if session_id == count - 1:
        time.sleep(0.05)

finished = 0
for line in sys.stdin:
    command = json.loads(line)
    session_id = command["sessionId"]
    if command.get("cancel") is not True:
        print(json.dumps({
            "sessionId": session_id,
            "stage": "verify",
            "deviceToken": f"init-{session_id}",
            "verifyDeviceToken": f"verify-{session_id}",
        }), flush=True)
    finished += 1
    if finished == count:
        break
""".strip()

        with tempfile.TemporaryDirectory(prefix="ali-host-python-test-") as temp:
            bridge = Path(temp, "fake_host.py")
            sdk = Path(temp, "fixture-sdk.js")
            bridge.write_text(bridge_source, encoding="utf-8")
            sdk.write_text("// fixture", encoding="utf-8")
            client = DeviceRuntimeClient(
                node_binary=sys.executable,
                bridge_script=bridge,
                sdk_path=sdk,
                timeout=2.0,
                device_profile=generate_device_profile(),
            )

            def fake_result(payload, **_kwargs):
                return SimpleNamespace(
                    init_token=payload["deviceToken"],
                    payload=payload,
                )

            with mock.patch.object(
                node_device, "_build_result", side_effect=fake_result
            ):
                sessions = client.open_challenge_sessions(2)
                self.assertIs(sessions[0].process, sessions[1].process)
                self.assertLess(
                    sessions[1].initialized_at,
                    sessions[0].initialized_at,
                )
                results: dict[int, object] = {}
                errors: list[Exception] = []

                def complete(index: int) -> None:
                    try:
                        results[index] = sessions[index].complete_challenge(
                            (f"getter-{index}",),
                            (
                                {
                                    "type": "mousemove",
                                    "x": index + 1,
                                    "y": 2,
                                    "timeStamp": 10,
                                    "isTrusted": True,
                                },
                            ),
                            post_interaction_delay_ms=0,
                        )
                    except Exception as exc:  # pragma: no cover
                        errors.append(exc)

                workers = [
                    threading.Thread(target=complete, args=(index,))
                    for index in range(2)
                ]
                for worker in workers:
                    worker.start()
                for worker in workers:
                    worker.join(timeout=3.0)

                try:
                    self.assertEqual(errors, [])
                    self.assertEqual(
                        results[0].payload["verifyDeviceToken"], "verify-0"
                    )
                    self.assertEqual(
                        results[1].payload["verifyDeviceToken"], "verify-1"
                    )
                finally:
                    for session in sessions:
                        session.close()

    def test_python_host_recycles_one_session_without_replacing_process(
        self,
    ) -> None:
        bridge_source = """
import json
import sys

args = sys.argv[1:]
count = int(args[args.index("--vm-count") + 1])
for session_id in range(count):
    print(json.dumps({
        "sessionId": session_id,
        "stage": "init",
        "deviceToken": f"init-{session_id}-1",
    }), flush=True)

generations = [1] * count
for line in sys.stdin:
    command = json.loads(line)
    session_id = command["sessionId"]
    if command.get("reset") is True:
        generations[session_id] += 1
        print(json.dumps({
            "sessionId": session_id,
            "stage": "init",
            "deviceToken": f"init-{session_id}-{generations[session_id]}",
        }), flush=True)
    elif command.get("cancel") is not True:
        print(json.dumps({
            "sessionId": session_id,
            "stage": "verify",
            "deviceToken": f"init-{session_id}-{generations[session_id]}",
            "verifyDeviceToken": f"verify-{session_id}-{generations[session_id]}",
        }), flush=True)
""".strip()

        with tempfile.TemporaryDirectory(prefix="ali-host-reset-python-test-") as temp:
            bridge = Path(temp, "fake_host.py")
            sdk = Path(temp, "fixture-sdk.js")
            bridge.write_text(bridge_source, encoding="utf-8")
            sdk.write_text("// fixture", encoding="utf-8")
            client = DeviceRuntimeClient(
                node_binary=sys.executable,
                bridge_script=bridge,
                sdk_path=sdk,
                timeout=2.0,
                device_profile=generate_device_profile(),
            )

            def fake_result(payload, **_kwargs):
                return SimpleNamespace(
                    init_token=payload["deviceToken"],
                    payload=payload,
                )

            with mock.patch.object(
                node_device, "_build_result", side_effect=fake_result
            ):
                sessions = client.open_challenge_sessions(2)
                process = sessions[0].process
                try:
                    first = sessions[0].complete_challenge(
                        ("getter-first",),
                        (
                            {
                                "type": "mousemove",
                                "x": 1,
                                "y": 2,
                                "timeStamp": 10,
                                "isTrusted": True,
                            },
                        ),
                        post_interaction_delay_ms=0,
                    )
                    self.assertEqual(
                        first.payload["verifyDeviceToken"], "verify-0-1"
                    )

                    sessions[0].recycle()
                    self.assertIs(sessions[0].process, process)
                    self.assertIs(sessions[1].process, process)
                    self.assertEqual(sessions[0].init_token, "init-0-2")

                    second = sessions[0].complete_challenge(
                        ("getter-second",),
                        (
                            {
                                "type": "mousemove",
                                "x": 3,
                                "y": 4,
                                "timeStamp": 20,
                                "isTrusted": True,
                            },
                        ),
                        post_interaction_delay_ms=0,
                    )
                    self.assertEqual(
                        second.payload["verifyDeviceToken"], "verify-0-2"
                    )
                    self.assertEqual(sessions[1].init_token, "init-1-1")
                finally:
                    for session in sessions:
                        session.close()


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

            def run_captcha(
                self,
                runtime,
                *,
                minimum_confidence,
                timing_hook,
                cancel_check,
            ):
                self.runtime = runtime
                timing_hook("deviceSession", 12)
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
        self.assertEqual(result["timingsMs"]["deviceSession"], 12)
        self.assertEqual(result["elapsedMs"], result["timingsMs"]["total"])

    def test_fast_transport_adapter_is_forwarded_by_route(self) -> None:
        profile = object()
        adapter = object()
        proxies = {"https": "http://proxy.invalid:8080"}
        seen: list[object] = []
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

            def run_captcha(
                self,
                runtime,
                *,
                minimum_confidence,
                timing_hook,
                cancel_check,
            ):
                timing_hook("deviceSession", 7)
                return SimpleNamespace(verify=verify)

            def close(self) -> None:
                return None

        class Settings:
            minimum_confidence = 0.45

            @staticmethod
            def build_client(*, device_profile, adapter, **kwargs):
                seen.append(adapter)
                return Client()

            @staticmethod
            def build_device_runtime(*, device_profile, **kwargs):
                return object()

        class TransportPool:
            @staticmethod
            def get(value):
                self.assertEqual(value, proxies)
                return adapter

        request = SolveRequest(
            scene_id="scene",
            proxies=proxies,
            rpc_key_id=None,
            prefix="default",
        )

        result = solve_once(
            request,
            Settings(),
            device_profile=profile,
            transport_pool=TransportPool(),
        )

        self.assertTrue(result["ok"])
        self.assertEqual(seen, [adapter])


class SharedTransportPoolTests(unittest.TestCase):
    class _Adapter:
        def __init__(self, *, pool_connections: int, pool_maxsize: int) -> None:
            self.pool_connections = pool_connections
            self.pool_maxsize = pool_maxsize
            self.closed = 0

        def close(self) -> None:
            self.closed += 1

    class _Requests:
        adapters = SimpleNamespace(HTTPAdapter=None)

    def setUp(self) -> None:
        self._Requests.adapters.HTTPAdapter = self._Adapter

    def test_same_route_reuses_adapter_and_proxy_routes_are_isolated(self) -> None:
        pool = SharedHttpAdapterPool(
            self._Requests,
            pool_size=7,
            max_proxy_routes=2,
        )
        direct = pool.get(None)
        proxy_a = pool.get(
            {"https": "http://one.invalid:8080", "http": "http://one.invalid:8080"}
        )

        self.assertIs(pool.get({}), direct)
        self.assertIs(
            pool.get(
                {"http": "http://one.invalid:8080", "https": "http://one.invalid:8080"}
            ),
            proxy_a,
        )
        self.assertIsNot(
            pool.get({"https": "http://two.invalid:8080"}),
            proxy_a,
        )
        self.assertIsNone(pool.get({"https": "http://three.invalid:8080"}))
        self.assertEqual(direct.pool_connections, 7)

        adapters = tuple(pool._adapters.values())
        pool.close()
        pool.close()
        self.assertTrue(all(item.closed == 1 for item in adapters))
        with self.assertRaises(RuntimeError):
            pool.get(None)

    def test_client_close_detaches_borrowed_adapter(self) -> None:
        borrowed = self._Adapter(pool_connections=8, pool_maxsize=8)
        local = self._Adapter(pool_connections=1, pool_maxsize=1)

        class Session:
            def __init__(self) -> None:
                self.adapters = {"https://": borrowed, "http://": local}

            def close(self) -> None:
                for adapter in set(self.adapters.values()):
                    adapter.close()

        client = object.__new__(AliSliderClient)
        client.vision = SimpleNamespace(close=lambda: None)
        client.session = Session()
        client._adapter = borrowed
        client._owns_adapter = False

        client.close()

        self.assertEqual(borrowed.closed, 0)
        self.assertEqual(local.closed, 1)

    def test_startup_warmup_fills_each_host_to_concurrency(self) -> None:
        calls: list[tuple[object, str, float]] = []

        def record(adapter: object, url: str, timeout: float) -> None:
            calls.append((adapter, url, timeout))

        adapter = object()
        with mock.patch(
            "ali_slider_reverse.challenge.transport._warm_one",
            side_effect=record,
        ):
            warm_connections(
                adapter,
                ("https://one.invalid/", "https://one.invalid/", "https://two.invalid/"),
                connections_per_url=3,
                wait=True,
            )

        self.assertEqual(len(calls), 6)
        self.assertEqual(
            sorted(url for _, url, _ in calls),
            ["https://one.invalid/"] * 3 + ["https://two.invalid/"] * 3,
        )

    def test_disabled_upload_log_is_not_scheduled(self) -> None:
        verify = SimpleNamespace(succeeded=True)
        completed_device = SimpleNamespace(
            getter_argument_count=1,
            verify_token="verify-token",
        )
        build = SimpleNamespace(
            device_getter_arguments=("arg",),
            feilin_interaction_events=({"type": "mousemove"},),
            post_interaction_delay_ms=0.0,
        )

        class DeviceSession:
            init_token = "init-token"
            initial_result = SimpleNamespace()
            sdk_path = "sdk.js"
            target_first_touch_age_ms = 700

            @staticmethod
            def complete_challenge(*args, **kwargs):
                return completed_device

        class Runtime:
            @staticmethod
            @contextmanager
            def challenge_session():
                yield DeviceSession()

        client = object.__new__(AliSliderClient)
        client.prewarm_connections = lambda: None
        client.init_challenge = lambda token: SimpleNamespace(init_started_ms=1)
        client.download_assets = lambda challenge, directory: SimpleNamespace()
        client.upload_initialization_log = mock.Mock(
            side_effect=AssertionError("UploadLog must stay disabled")
        )
        client.solve_assets = lambda assets, x_pos_override=None: SimpleNamespace(
            confidence=1.0
        )
        client.build_verify_data = lambda *args, **kwargs: build
        client.verify_challenge = lambda *args, **kwargs: verify

        client._upload_executor = None
        with (
            mock.patch.object(config, "UPLOAD_LOG_ENABLED", False),
            mock.patch(
                "ali_slider_reverse.challenge.session.ThreadPoolExecutor"
            ) as executor,
        ):
            started = time.monotonic()
            timings: dict[str, int] = {}
            outcome = AliSliderClient.run_captcha(
                client,
                Runtime(),
                timing_hook=timings.__setitem__,
            )
            elapsed = time.monotonic() - started

        self.assertLess(elapsed, 0.5)
        self.assertFalse(outcome.upload_log_succeeded)
        client.upload_initialization_log.assert_not_called()
        executor.assert_not_called()
        self.assertEqual(
            tuple(timings),
            (
                "deviceSession",
                "init",
                "downloadAssets",
                "vision",
                "buildVerifyData",
                "completeDevice",
                "verify",
                "uploadLog",
            ),
        )
        self.assertTrue(all(value >= 0 for value in timings.values()))

    def test_cancel_check_stops_before_next_protocol_stage(self) -> None:
        class Runtime:
            @staticmethod
            @contextmanager
            def challenge_session():
                yield object()

        checks = 0
        timings: dict[str, int] = {}

        def cancel_after_device_session() -> None:
            nonlocal checks
            checks += 1
            if checks == 2:
                raise RuntimeError("client disconnected")

        client = object.__new__(AliSliderClient)
        client.prewarm_connections = lambda: None
        client.init_challenge = mock.Mock()

        with self.assertRaisesRegex(RuntimeError, "client disconnected"):
            AliSliderClient.run_captcha(
                client,
                Runtime(),
                timing_hook=timings.__setitem__,
                cancel_check=cancel_after_device_session,
            )

        self.assertEqual(tuple(timings), ("deviceSession",))
        client.init_challenge.assert_not_called()

    def test_socket_disconnect_is_detected_without_blocking(self) -> None:
        import socket

        server_side, client_side = socket.socketpair()
        self.addCleanup(server_side.close)

        self.assertFalse(api._connection_closed(server_side))
        client_side.close()
        self.assertTrue(api._connection_closed(server_side))

    def test_concurrency_gate_reports_backoff_from_recent_slow_request(
        self,
    ) -> None:
        slots = api._ChallengeSlots(1)
        self.assertTrue(slots.acquire(blocking=False))
        self.assertFalse(slots.acquire(blocking=False))
        first = slots.overload_info(fallback_ms=25_000)
        self.assertEqual(first["activeChallenges"], 1)
        self.assertEqual(first["maxConcurrency"], 1)
        slots.release(elapsed_ms=40_000)

        self.assertTrue(slots.acquire(blocking=False))
        second = slots.overload_info(fallback_ms=25_000)
        slots.release()
        self.assertGreaterEqual(second["retryAfterMs"], 39_000)
        self.assertGreaterEqual(second["recommendedClientTimeoutMs"], 60_000)

    def test_disabled_upload_log_method_returns_without_network(self) -> None:
        client = object.__new__(AliSliderClient)
        client._post_rpc = mock.Mock(
            side_effect=AssertionError("UploadLog network call is forbidden")
        )

        with mock.patch.object(config, "UPLOAD_LOG_ENABLED", False):
            result = AliSliderClient.upload_initialization_log(
                client, object(), object()
            )

        self.assertFalse(result)
        client._post_rpc.assert_not_called()

    def test_disabled_upload_host_is_not_prewarmed(self) -> None:
        with mock.patch.object(config, "UPLOAD_LOG_ENABLED", False):
            urls = api._startup_warm_urls()

        self.assertNotIn(config.UPLOAD_URL, urls)
        self.assertIn(config.init_url(), urls)
        self.assertIn(config.verify_url(), urls)


class ApiDocumentationTests(unittest.TestCase):
    @staticmethod
    @contextmanager
    def _running_server(handler: type[SliderApiHandler]):
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        server.daemon_threads = True
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            host, port = server.server_address[:2]
            yield f"http://{host}:{port}"
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=2.0)

    def test_docs_and_openapi_are_served_locally(self) -> None:
        class Handler(SliderApiHandler):
            result_hook = None

        with self._running_server(Handler) as base_url:
            with urllib.request.urlopen(base_url + "/docs") as response:
                docs = response.read().decode("utf-8")
            with urllib.request.urlopen(base_url + "/openapi.json") as response:
                spec = json.loads(response.read().decode("utf-8"))

        self.assertIn("AliSlider API", docs)
        self.assertIn(config.API_SOLVE_PATH, docs)
        self.assertIn("完整响应", docs)
        self.assertIn("timingsMs", docs)
        self.assertIn(config.API_SOLVE_PATH, spec["paths"])
        responses = spec["paths"][config.API_SOLVE_PATH]["post"]["responses"]
        success_properties = responses["200"]["content"]["application/json"][
            "schema"
        ]["properties"]
        self.assertIn("timingsMs", success_properties)
        self.assertIn("Retry-After", responses["429"]["headers"])

    def test_api_response_and_console_hook_receive_full_json(self) -> None:
        captured: list[tuple[str, str, int, dict[str, object]]] = []

        class Handler(SliderApiHandler):
            pass

        Handler.result_hook = staticmethod(
            lambda method, path, status, body: captured.append(
                (method, path, status, body)
            )
        )
        expected = {
            "ok": True,
            "securityToken": "complete-security-token",
            "VerifyCode": "T001",
            "VerifyResult": True,
            "certifyId": "certify-id",
            "sceneId": config.DEFAULT_SCENE_ID,
            "proxied": False,
            "elapsedMs": 1234,
            "timingsMs": {"total": 1234},
            "traceId": "trace123",
        }
        payload = json.dumps({"SceneId": config.DEFAULT_SCENE_ID}).encode()

        with (
            mock.patch.object(api, "solve_once", return_value=dict(expected)),
            mock.patch.object(api.secrets, "token_hex", return_value="trace123"),
            self._running_server(Handler) as base_url,
        ):
            request = urllib.request.Request(
                base_url + config.API_SOLVE_PATH,
                data=payload,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(request) as response:
                actual = json.loads(response.read().decode("utf-8"))

        self.assertEqual(actual, expected)
        self.assertEqual(captured, [("POST", config.API_SOLVE_PATH, 200, expected)])

        output = io.StringIO()
        with redirect_stderr(output):
            api._print_full_result("POST", config.API_SOLVE_PATH, 200, expected)
        self.assertIn("complete-security-token", output.getvalue())
        self.assertIn('"VerifyCode":"T001"', output.getvalue())

    def test_overload_response_includes_retry_header_and_capacity(self) -> None:
        slots = api._ChallengeSlots(1)
        self.assertTrue(slots.acquire(blocking=False))

        class Handler(SliderApiHandler):
            result_hook = None
            progress_hook = None

        Handler.slots = slots
        payload = json.dumps({"SceneId": config.DEFAULT_SCENE_ID}).encode()
        try:
            with self._running_server(Handler) as base_url:
                request = urllib.request.Request(
                    base_url + config.API_SOLVE_PATH,
                    data=payload,
                    headers={"Content-Type": "application/json"},
                    method="POST",
                )
                with self.assertRaises(urllib.error.HTTPError) as raised:
                    urllib.request.urlopen(request)
                error = raised.exception
                body = json.loads(error.read().decode("utf-8"))
                error.close()
        finally:
            slots.release()

        self.assertEqual(error.code, 429)
        self.assertGreaterEqual(int(error.headers["Retry-After"]), 1)
        self.assertEqual(body["activeChallenges"], 1)
        self.assertEqual(body["maxConcurrency"], 1)
        self.assertIn("recommendedClientTimeoutMs", body)

    def test_five_disconnected_requests_release_all_concurrency_slots(
        self,
    ) -> None:
        import socket

        slots = api._ChallengeSlots(5)
        entered = threading.Barrier(6)
        continue_checks = threading.Event()

        class Handler(SliderApiHandler):
            result_hook = None
            progress_hook = None

        Handler.slots = slots

        def wait_for_disconnect(
            *args,
            timing_hook,
            cancel_check,
            **kwargs,
        ):
            timing_hook("setup", 1)
            entered.wait(timeout=2.0)
            continue_checks.wait(timeout=2.0)
            cancel_check()
            raise AssertionError("断连检查必须在此处停止请求")

        clients: list[socket.socket] = []
        with (
            mock.patch.object(api, "solve_once", side_effect=wait_for_disconnect),
            self._running_server(Handler) as base_url,
        ):
            parts = urllib.parse.urlsplit(base_url)
            try:
                for _ in range(5):
                    client = socket.create_connection(
                        (parts.hostname or "127.0.0.1", parts.port or 80)
                    )
                    client.sendall(
                        (
                            "POST /api/slider HTTP/1.1\r\n"
                            f"Host: {parts.netloc}\r\n"
                            "Content-Type: application/json\r\n"
                            "Content-Length: 2\r\n\r\n{}"
                        ).encode()
                    )
                    clients.append(client)

                entered.wait(timeout=2.0)
                self.assertEqual(
                    slots.overload_info(fallback_ms=25_000)[
                        "activeChallenges"
                    ],
                    5,
                )
                for client in clients:
                    client.close()
                continue_checks.set()

                deadline = time.monotonic() + 2.0
                while time.monotonic() < deadline:
                    if (
                        slots.overload_info(fallback_ms=25_000)[
                            "activeChallenges"
                        ]
                        == 0
                    ):
                        break
                    time.sleep(0.01)
                self.assertEqual(
                    slots.overload_info(fallback_ms=25_000)[
                        "activeChallenges"
                    ],
                    0,
                )
            finally:
                continue_checks.set()
                for client in clients:
                    client.close()


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

    def test_fast_parser_defaults_to_three_vms_per_node(self) -> None:
        default_args = api.build_parser().parse_args(["--runtime-mode", "fast"])
        single_vm_args = api.build_parser().parse_args(
            ["--runtime-mode", "fast", "--vms-per-node", "1"]
        )

        self.assertEqual(default_args.vms_per_node, 3)
        self.assertEqual(single_vm_args.vms_per_node, 1)
        with self.assertRaisesRegex(SystemExit, "--vms-per-node"):
            api.main(["--vms-per-node", "0"])

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

    def test_no_argument_main_starts_fast_api_and_opens_docs(self) -> None:
        with (
            mock.patch.object(desktop, "_prepare_console") as prepare,
            mock.patch.object(desktop, "_select_startup_arguments") as choose,
            mock.patch.object(desktop, "run_console") as run,
            mock.patch.object(desktop, "api_main", return_value=17) as api_run,
        ):
            result = desktop.main([])

        self.assertEqual(result, 17)
        prepare.assert_called_once()
        choose.assert_not_called()
        run.assert_not_called()
        api_run.assert_called_once_with(
            [
                "--runtime-mode",
                "fast",
                "--max-concurrency",
                "5",
                "--open-docs",
            ]
        )


if __name__ == "__main__":
    unittest.main()
