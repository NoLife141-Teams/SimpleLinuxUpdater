import importlib.util
import os
from pathlib import Path
import subprocess
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("recovery_smoke", Path(__file__).with_name("admin-recovery-smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class LocalDockerSelectionTests(unittest.TestCase):
    def test_standard_and_managed_local_socket_are_pinned_without_connection(self):
        for endpoint in ("unix:///var/run/docker.sock", "unix:///home/runner/setup-docker-action/run-11665110/docker.sock"):
            with self.subTest(endpoint=endpoint), mock.patch.object(smoke.subprocess, "run") as execute:
                command = smoke.selected_local_docker(endpoint)
                self.assertEqual(command, ["docker", "--host=" + endpoint])
                execute.assert_not_called()

    def test_remote_and_malformed_endpoints_are_rejected_without_connection(self):
        for endpoint in ("tcp://localhost:2375", "tcp://production.example:2376", "ssh://production.example",
                         "unix://remote/docker.sock", "unix:relative.sock", "unix:///tmp/docker.sock?remote=1",
                         "unix:///tmp/docker.sock\n", "npipe:////./pipe/docker_engine"):
            with self.subTest(endpoint=endpoint), mock.patch.object(smoke.subprocess, "run") as execute:
                with self.assertRaises(ValueError):
                    smoke.selected_local_docker(endpoint)
                execute.assert_not_called()

    def test_current_context_is_inspected_once_and_its_endpoint_is_pinned(self):
        endpoint = "unix:///home/runner/setup-docker-action/run-fixture/docker.sock"
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(smoke.subprocess, "run", return_value=
                subprocess.CompletedProcess([], 0, stdout=(endpoint + "\n").encode())) as execute:
            command = smoke.selected_local_docker()
        execute.assert_called_once()
        self.assertEqual(execute.call_args.args[0][:3], ["docker", "context", "inspect"])
        self.assertEqual(command, ["docker", "--host=" + endpoint])

    def test_remote_environment_is_rejected_before_context_or_daemon_calls(self):
        with mock.patch.dict(os.environ, {"DOCKER_HOST": "ssh://production.example"}, clear=True), mock.patch.object(smoke.subprocess, "run") as execute:
            with self.assertRaises(ValueError):
                smoke.selected_local_docker()
            execute.assert_not_called()

    def test_remote_context_is_rejected_after_metadata_inspection_only(self):
        with mock.patch.dict(os.environ, {"DOCKER_CONTEXT": "remote"}, clear=True), mock.patch.object(smoke.subprocess, "run", return_value=
                subprocess.CompletedProcess([], 0, stdout=b"ssh://production.example\n")) as execute:
            with self.assertRaises(ValueError):
                smoke.selected_local_docker()
        execute.assert_called_once()
        self.assertEqual(execute.call_args.args[0][:3], ["docker", "context", "inspect"])


if __name__ == "__main__": unittest.main()
