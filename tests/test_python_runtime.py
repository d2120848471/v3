"""纯 Python Device/PE 协议的离线回归。"""

from __future__ import annotations

import base64
import json
import unittest
import urllib.parse
from contextlib import contextmanager
from unittest import mock

from ali_slider_reverse.device_profile import generate_device_profile
from ali_slider_reverse.entrypoints.options import RuntimeSettings
from ali_slider_reverse.errors import DeviceRuntimeError, PeRuntimeError
from ali_slider_reverse.protocol.data_codec import pack_data, unpack_data
from ali_slider_reverse.protocol.device_token import (
    aes_cbc_decrypt_base64,
    aes_cbc_encrypt_base64,
    parse_device_token,
)
from ali_slider_reverse.protocol.secrets import FrontendSecrets
from ali_slider_reverse.runtime.device import (
    DeviceRuntimeClient,
    DeviceRuntimeSession,
)
from ali_slider_reverse.runtime.pe import PeRuntimeClient

_AES_KEY = "0123456789abcdef"
_SESSION_KEY = "fedcba9876543210"
_TOKEN_SALT = "offline-token-salt"
_SECRETS = FrontendSecrets(
    main_rpc_key_id="offline-main-id",
    main_rpc_key_secret="offline-main-secret",
    device_token_salt=_TOKEN_SALT,
    device_rpc_key_id="offline-device-id",
    device_rpc_key_secret="offline-device-secret",
    device_request_key=_AES_KEY,
    device_response_key=_AES_KEY,
    device_flag_key=_AES_KEY,
    device_upload_key=_AES_KEY,
    device_preid_key=_AES_KEY,
)


class _Clock:
    def __init__(self, milliseconds: int = 1_700_000_000_000) -> None:
        self.milliseconds = milliseconds

    def time(self) -> float:
        return self.milliseconds / 1000

    def monotonic(self) -> float:
        return self.time()

    def sleep(self, seconds: float) -> None:
        self.milliseconds += round(seconds * 1000)


class _Response:
    def __init__(self, payload: dict[str, object]) -> None:
        self._payload = payload

    def raise_for_status(self) -> None:
        return None

    def json(self) -> dict[str, object]:
        return self._payload


class _OfflineDeviceHttp:
    def __init__(self, *, session_id: str, clock: _Clock) -> None:
        self.session_id = session_id
        self.clock = clock
        self.closed = False
        self.requests: list[dict[str, str]] = []
        self.config_timestamp: int | None = None

    def post(self, _url: str, *, data: str, **_kwargs: object) -> _Response:
        form = urllib.parse.parse_qs(
            data,
            keep_blank_values=True,
            strict_parsing=True,
        )
        action = form["Action"][0]
        self.requests.append({"action": action, "data": form["Data"][0]})
        if action != "Log1":
            return _Response({"Code": "200"})

        self.config_timestamp = self.clock.milliseconds + 10
        def encoded(value: str) -> str:
            return base64.b64encode(value.encode()).decode()

        plaintext = "#".join(
            (
                encoded(_SESSION_KEY),
                encoded("1"),
                self.session_id,
                "1.5.1",
                "",
                "",
                "",
                str(self.config_timestamp),
                "203.0.113.8",
                "1",
            )
        )
        ciphertext = aes_cbc_encrypt_base64(
            plaintext,
            key=_SECRETS.device_response_key,
        )
        return _Response(
            {"Code": "200", "ResultObject": {"DeviceConfig": ciphertext}}
        )

    def close(self) -> None:
        self.closed = True


class _FailingDeviceHttp:
    def __init__(self) -> None:
        self.closed = False

    def post(self, *_args: object, **_kwargs: object) -> _Response:
        raise RuntimeError("offline failure")

    def close(self) -> None:
        self.closed = True


@contextmanager
def _patched_device_runtime(
    clock: _Clock,
    http_sessions: list[_OfflineDeviceHttp],
):
    with (
        mock.patch(
            "ali_slider_reverse.runtime.device.resolve_frontend_secrets",
            return_value=_SECRETS,
        ),
        mock.patch(
            "ali_slider_reverse.runtime.device.time.time",
            side_effect=clock.time,
        ),
        mock.patch(
            "ali_slider_reverse.runtime.device.time.monotonic",
            side_effect=clock.monotonic,
        ),
        mock.patch(
            "ali_slider_reverse.runtime.device.time.sleep",
            side_effect=clock.sleep,
        ),
        mock.patch.object(
            DeviceRuntimeSession,
            "_new_http",
            side_effect=http_sessions,
        ),
    ):
        yield


def _decrypt_token_fields(token: str) -> list[str]:
    parsed = parse_device_token(token, salt=_TOKEN_SALT)
    plaintext = aes_cbc_decrypt_base64(
        parsed.fingerprint_cipher,
        key=_SESSION_KEY,
    ).decode()
    return plaintext.split("#")


def _decode_log2(data: str) -> tuple[list[str], list[str], list[str]]:
    outer = aes_cbc_decrypt_base64(data, key=_SECRETS.device_upload_key).decode()
    outer_fields = outer.split("#")
    inner = base64.b64decode(outer_fields[7], validate=True).decode()
    inner_fields = inner.split("#")
    fingerprint = aes_cbc_decrypt_base64(
        inner_fields[1],
        key=_SESSION_KEY,
    ).decode()
    return outer_fields, inner_fields, fingerprint.split("#")


class PurePythonDeviceRuntimeTests(unittest.TestCase):
    def _client(self) -> DeviceRuntimeClient:
        return DeviceRuntimeClient(
            endpoint="https://example.invalid/device",
            timeout=1.0,
            gather_cost_range=(200, 200),
            first_touch_age_range=(700, 700),
            device_profile=generate_device_profile(),
        )

    def test_offline_device_chain_and_payloads_self_validate(self) -> None:
        clock = _Clock()
        http = _OfflineDeviceHttp(
            session_id="offline-session-ABCDEFGH",
            clock=clock,
        )
        client = self._client()

        with (
            _patched_device_runtime(clock, [http]),
            client.challenge_session() as session,
        ):
            result = session.complete_challenge(
                ("certify-id-1234",),
                (
                    {
                        "type": "mousemove",
                        "x": 101,
                        "y": 202,
                        "timeStamp": 700,
                        "isTrusted": True,
                    },
                ),
                post_interaction_delay_ms=0,
            )

        self.assertTrue(http.closed)
        self.assertEqual(
            [request["action"] for request in http.requests],
            ["Log1", "Log2", "Log3", "Log2"],
        )
        self.assertEqual(
            result.request_actions,
            ("Log1", "Log2", "Log3", "Log2"),
        )
        self.assertEqual(result.fingerprint_field_count, 111)
        self.assertEqual(result.getter_argument_count, 1)
        self.assertEqual(result.interaction_event_count, 1)
        self.assertEqual(
            result.init_parsed.gather_cost,
            result.verify_parsed.gather_cost,
        )

        init_fields = _decrypt_token_fields(result.init_token)
        verify_fields = _decrypt_token_fields(result.verify_token)
        self.assertEqual(len(init_fields), 111)
        self.assertEqual(len(verify_fields), 111)
        self.assertEqual(init_fields[21], "LC1eMzMzYDU=")
        self.assertEqual(init_fields[77], "")
        self.assertEqual(verify_fields[77], "certify-id-1234")
        self.assertEqual(init_fields[73], verify_fields[73])
        self.assertNotEqual(init_fields[71], verify_fields[71])
        self.assertIn("93-", init_fields[43])
        self.assertIn("94-", verify_fields[43])

        log1 = aes_cbc_decrypt_base64(
            http.requests[0]["data"],
            key=_SECRETS.device_request_key,
        ).decode()
        self.assertTrue(log1.startswith("ab034ec0643f91399eb33e062dc7fae1#W#"))
        for request_index, token_fields in ((1, init_fields), (3, verify_fields)):
            outer, inner, log2_fields = _decode_log2(
                http.requests[request_index]["data"]
            )
            self.assertEqual(len(outer), 8)
            self.assertEqual(outer[5:7], ["0", "501"])
            self.assertEqual(len(inner), 6)
            self.assertEqual(inner[0], "offline-session-ABCDEFGH")
            self.assertEqual(len(log2_fields), 111)
            self.assertNotIn("93-", log2_fields[43])
            self.assertNotIn("94-", log2_fields[43])
            self.assertEqual(
                log2_fields[:43] + log2_fields[44:],
                token_fields[:43] + token_fields[44:],
            )

        log3_outer = aes_cbc_decrypt_base64(
            http.requests[2]["data"],
            key=_SECRETS.device_upload_key,
        ).decode().split("#")
        self.assertEqual(len(log3_outer), 7)
        stage = base64.b64decode(log3_outer[6], validate=True).decode()
        marker_text, combat_text = stage.removeprefix("511#").split("-504#", 1)
        marker = base64.b64decode(marker_text, validate=True).decode().split("#")
        combat = base64.b64decode(combat_text, validate=True).decode().split("#")
        event = json.loads(
            aes_cbc_decrypt_base64(combat[1], key=_SESSION_KEY).decode()
        )
        self.assertEqual(marker[0], "offline-session-ABCDEFGH")
        self.assertEqual(combat[0], marker[0])
        self.assertEqual(
            tuple(event),
            (
                "mousemove",
                "mouseclick",
                "keyup",
                "scrollTop",
                "scrollLeft",
                "pointerEvent",
                "clientType",
                "startTime",
                "timestamp",
            ),
        )
        self.assertTrue(all(event[name] == [] for name in tuple(event)[:6]))
        self.assertEqual(event["clientType"], "mobile")
        self.assertEqual(event["timestamp"], str(http.config_timestamp))
        self.assertEqual(event["startTime"] - http.config_timestamp, 90)

    def test_recycle_replaces_config_without_leaking_previous_session(self) -> None:
        clock = _Clock()
        first_http = _OfflineDeviceHttp(
            session_id="offline-session-ABCDEFGH",
            clock=clock,
        )
        second_http = _OfflineDeviceHttp(
            session_id="offline-session-IJKLMNOP",
            clock=clock,
        )
        session = DeviceRuntimeSession(self._client())

        with _patched_device_runtime(clock, [first_http, second_http]):
            session.__enter__()
            first_token = session.init_token
            first_session_id = session.initial_result.device_config.session_id
            session.recycle()
            second_token = session.init_token
            second_session_id = session.initial_result.device_config.session_id
            session.close()

        self.assertTrue(first_http.closed)
        self.assertTrue(second_http.closed)
        self.assertNotEqual(first_token, second_token)
        self.assertEqual(first_session_id, "offline-session-ABCDEFGH")
        self.assertEqual(second_session_id, "offline-session-IJKLMNOP")
        self.assertEqual(
            session.initial_result.request_actions,
            ("Log1", "Log2", "Log3"),
        )

    def test_open_sessions_keep_tokens_and_http_state_isolated(self) -> None:
        clock = _Clock()
        http_sessions = [
            _OfflineDeviceHttp(
                session_id="offline-session-ABCDEFGH",
                clock=clock,
            ),
            _OfflineDeviceHttp(
                session_id="offline-session-IJKLMNOP",
                clock=clock,
            ),
        ]
        sessions = self._client().open_challenge_sessions(2)

        with _patched_device_runtime(clock, http_sessions):
            try:
                for session in sessions:
                    session.__enter__()
                self.assertIsNot(sessions[0]._http, sessions[1]._http)
                self.assertNotEqual(sessions[0].init_token, sessions[1].init_token)
                self.assertNotEqual(
                    sessions[0].initial_result.device_config.session_id,
                    sessions[1].initial_result.device_config.session_id,
                )
                self.assertEqual(
                    sessions[0].initial_result.request_actions,
                    ("Log1", "Log2", "Log3"),
                )
                self.assertEqual(
                    sessions[1].initial_result.request_actions,
                    ("Log1", "Log2", "Log3"),
                )
            finally:
                for session in sessions:
                    session.close()

        self.assertTrue(all(http.closed for http in http_sessions))

    def test_initialization_failure_closes_partial_http_session(self) -> None:
        clock = _Clock()
        http = _FailingDeviceHttp()
        session = DeviceRuntimeSession(self._client())

        with _patched_device_runtime(clock, [http]):
            with self.assertRaises(DeviceRuntimeError):
                session.__enter__()

        self.assertTrue(http.closed)
        self.assertIsNone(session._http)
        self.assertFalse(session.recyclable)
        with self.assertRaises(DeviceRuntimeError):
            session.recycle()


class PurePythonPeRuntimeTests(unittest.TestCase):
    def test_pe_data_round_trip_preserves_track_and_getter_contract(self) -> None:
        runtime = PeRuntimeClient(device_profile=generate_device_profile())
        track = [
            {"type": "touchstart", "x": 0, "y": 0, "dt": 0},
            {"type": "touchmove", "x": 30, "y": 1, "dt": 20},
            {"type": "touchmove", "x": 80, "y": 0, "dt": 20},
            {"type": "touchend", "x": 80, "y": 0, "dt": 10},
        ]

        with mock.patch(
            "ali_slider_reverse.runtime.pe.time.time",
            return_value=2_000_000_000,
        ):
            result = runtime.build(
                scene_id="scene-id",
                certify_id="0123456789abcdef",
                dimensions={"renderedWidth": 300, "handleWidth": 40},
                track=track,
                static_path="3.29.0/pe.091.00665af58b020d81.js",
                expected_x_pos=29,
                init_begin_time=1_999_999_995_000,
                first_touch_age_ms=700,
            )

        decoded = unpack_data(result.data)
        self.assertEqual(tuple(decoded.payload), result.payload_keys)
        self.assertEqual(
            tuple(decoded.payload["TrackList"]),
            result.track_keys,
        )
        self.assertEqual(result.slide_pos, 80)
        self.assertEqual(result.x_pos, 29)
        self.assertEqual(result.track_event_count, 4)
        self.assertEqual(result.data_mousemove_event_count, 5)
        self.assertEqual(
            len(decoded.payload["TrackList"]["mm"].split("|")),
            5,
        )
        self.assertEqual(
            decoded.payload["VerifyTime"]
            - decoded.payload["TrackStartTime"],
            750,
        )
        getter = result.device_getter_plans[0]
        self.assertEqual(getter.owner, "python-compatible.getToken")
        self.assertEqual(getter.argument_count, 1)
        self.assertEqual(getter.arguments[0].value, "0123456789abcdef")
        self.assertEqual(
            pack_data(
                decoded.payload,
                prefix=decoded.checksum_prefix,
            ),
            result.data,
        )

    def test_pe_rejects_ambiguous_numeric_inputs(self) -> None:
        runtime = PeRuntimeClient(device_profile=generate_device_profile())
        track = [
            {"type": "touchstart", "x": 0, "y": 0, "dt": 0},
            {"type": "touchmove", "x": 10, "y": 0, "dt": 10},
            {"type": "touchend", "x": 10, "y": 0, "dt": 10},
        ]
        with self.assertRaises(ValueError):
            runtime.build(
                scene_id="scene",
                certify_id="certify",
                dimensions={"renderedWidth": 300.5, "handleWidth": 40},
                track=track,
            )
        with self.assertRaises(PeRuntimeError):
            runtime.build(
                scene_id="scene",
                certify_id="certify",
                dimensions={"renderedWidth": 300, "handleWidth": 40},
                track=track,
                init_begin_time=0,
            )


class RuntimeSettingsTests(unittest.TestCase):
    def test_invalid_runtime_ranges_and_non_finite_values_fail_early(self) -> None:
        invalid_values = (
            {"timeout": float("nan")},
            {"minimum_confidence": float("inf")},
            {"gather_cost_range": (2, 1)},
            {"first_touch_age_range": (0, 1)},
        )
        for values in invalid_values:
            with self.subTest(values=values), self.assertRaises(ValueError):
                RuntimeSettings(**values)


if __name__ == "__main__":
    unittest.main()
