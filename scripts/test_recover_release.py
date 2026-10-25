import argparse
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("recover_release", Path(__file__).with_name("recover-release.py"))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class RecoverReleaseTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.state = Path(temporary.name)
        (self.state / "operations").mkdir()
        releases = self.state / "releases" / "application"
        releases.mkdir(parents=True)
        self.current = {"releaseId": "current", "containerId": "a" * 64, "roleContainers": ["a" * 64], "port": 3000}
        self.target = {"releaseId": "previous", "containerId": "b" * 64, "roleContainers": ["b" * 64], "port": 3001, "healthPath": "/_health"}
        recovery.atomic_json(self.state / "bindings.json", {"application": {"port": 3000, "metadata": self.current}})
        recovery.atomic_json(releases / "previous.json", self.target)
        self.args = argparse.Namespace(deployment="application", release="previous", expect_current="current",
                                       rook_service="rook-rehearsal", rook_binary="rook", acknowledge_operation=[])
        self.calls = []

    def command(self, *args):
        self.calls.append(args)
        responses = {
            ("rook", "--capabilities"): '["host-recovery-fence.v1"]',
            ("systemctl", "show", "rook-rehearsal", "--property=LoadState", "--value"): "loaded",
            ("systemctl", "show", "rook-rehearsal", "--property=ActiveState", "--value"): "inactive",
            ("docker", "inspect", "--format", "{{.Image}}", "b" * 64): "sha256:image",
            ("docker", "image", "inspect", "--format", "{{.Id}}", "sha256:image"): "sha256:image",
            ("docker", "inspect", "--format", "{{.State.Running}}", "b" * 64): "false",
            ("docker", "start", "b" * 64): "b" * 64,
            ("docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", "b" * 64): '{"3333/tcp":[{"HostIp":"127.0.0.1","HostPort":"3002"}]}',
            ("docker", "inspect", "--format", "{{.State.Running}} {{.State.Restarting}}", "b" * 64): "true false",
            ("docker", "stop", "--time", "60", "a" * 64): "a" * 64,
            ("docker", "stop", "--time", "30", "b" * 64): "b" * 64,
        }
        self.assertIn(args, responses, f"unexpected recovery command: {args}")
        return responses[args]

    def test_restore_uses_retained_containers_and_their_actual_port(self):
        with patch.object(recovery, "command", self.command), patch.object(recovery, "wait_healthy") as healthy:
            result = recovery.restore(self.args, self.state)

        binding = recovery.read_json(self.state / "bindings.json")["application"]
        self.assertEqual(binding["port"], 3002)
        self.assertEqual(binding["metadata"]["previousReleaseId"], "current")
        self.assertEqual(binding["metadata"]["recoveryVerification"], result["verification"])
        self.assertEqual(result["verification"]["releaseId"], "previous")
        self.assertEqual(result["verification"]["port"], 3002)
        self.assertTrue((self.state / "recovery-fence").exists())
        self.assertFalse(result["databaseRestored"])
        self.assertFalse(result["rookServiceStarted"])
        healthy.assert_called_once_with(3002, "/_health")
        self.assertEqual(sum(call[:2] == ("docker", "start") for call in self.calls), 1)

    def test_failed_readiness_preserves_binding_and_stops_only_restarted_processes(self):
        with patch.object(recovery, "command", self.command), patch.object(recovery, "wait_healthy", side_effect=ValueError("readiness failed")):
            with self.assertRaisesRegex(ValueError, "readiness failed"):
                recovery.restore(self.args, self.state)

        binding = recovery.read_json(self.state / "bindings.json")["application"]
        self.assertEqual(binding["metadata"]["releaseId"], "current")
        self.assertIn(("docker", "stop", "--time", "30", "b" * 64), self.calls)
        self.assertNotIn(("docker", "stop", "--time", "60", "a" * 64), self.calls)
        self.assertTrue((self.state / "recovery-fence").exists())

    def test_uncertain_operations_must_be_inspected_before_any_container_effect(self):
        recovery.atomic_json(self.state / "operations" / "migration.json", {"status": "interrupted"})

        with patch.object(recovery, "command", self.command):
            with self.assertRaisesRegex(ValueError, "acknowledge each uncertain operation: migration"):
                recovery.restore(self.args, self.state)

        self.assertFalse(any(call[0] == "docker" for call in self.calls))
        self.assertTrue((self.state / "recovery-fence").exists())

    def test_stale_recovery_request_cannot_replace_a_different_current_release(self):
        self.args.expect_current = "stale"

        with patch.object(recovery, "command", self.command):
            with self.assertRaisesRegex(ValueError, "current release changed"):
                recovery.restore(self.args, self.state)

        self.assertFalse(any(call[0] == "docker" for call in self.calls))


if __name__ == "__main__":
    unittest.main()
