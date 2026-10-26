import json
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import run


class HarnessSafetyTests(unittest.TestCase):
    def test_only_loopback_allocated_api_endpoints(self):
        for host, port in (("production.example", 2222), ("127.0.0.2", 2222), ("::1", 2222), ("127.0.0.1", 22), ("127.0.0.1", 65536)):
            with self.subTest(host=host, port=port), self.assertRaises(AssertionError):
                run.validate_local_endpoint(host, port)
        run.validate_local_endpoint("127.0.0.1", 30000)

    def test_environment_excludes_deployment_credentials_and_paths(self):
        with mock.patch.dict(os.environ, {"DEBIAN_UPDATER_DB_PATH": "/production/servers.db",
                "DEBIAN_UPDATER_MASTER_KEY": "secret", "SSH_AUTH_SOCK": "/personal/agent",
                "HTTP_PROXY": "http://credential@example.invalid", "HOME": "/personal"}):
            env = run.app_environment(Path("/private/fixture"), 8080)
        self.assertEqual(env["HOME"], "/private/fixture")
        self.assertEqual(env["DEBIAN_UPDATER_DB_PATH"], "/private/fixture/servers.db")
        for key in ("DEBIAN_UPDATER_MASTER_KEY", "SSH_AUTH_SOCK", "HTTP_PROXY"):
            self.assertNotIn(key, env)

    def test_unknown_distribution_never_creates_resource(self):
        with self.assertRaises(AssertionError):
            run.Harness("production.example", Path("unused"))

    def fake_harness(self):
        output = tempfile.TemporaryDirectory()
        self.addCleanup(output.cleanup)
        harness = run.Harness("debian13", Path(output.name))
        resources = {}
        calls = []

        def docker(*args, **kwargs):
            calls.append(args)
            if args[0] == "run":
                resources[("container", args[args.index("--name")+1])] = harness.owner
                raise RuntimeError("client disconnected after daemon created container")
            if args[0:2] == ("network", "create"):
                resources[("network", args[-1])] = harness.owner
                raise RuntimeError("client disconnected after daemon created network")
            kind = args[0]
            if args[1] == "inspect":
                key = (kind, args[-1])
                if key not in resources: raise RuntimeError("No such " + kind)
                return resources[key]
            if args[1] == "rm":
                resources.pop((kind, args[-1]))
                return ""
            raise AssertionError(args)

        harness.docker = docker
        return harness, resources, calls

    def test_cleanup_recovers_partial_create_with_missing_returned_ids(self):
        harness, resources, calls = self.fake_harness()
        # Model daemon-side success followed by client failure before assignments.
        with self.assertRaises(RuntimeError):
            harness.docker("run", "--name", harness.name, "fixture")
        with self.assertRaises(RuntimeError):
            harness.docker("network", "create", harness.network)
        resources[("container", harness.app_name)] = harness.owner
        resources[("image", harness.image)] = harness.owner
        harness.output = None
        self.assertIsNone(harness.container)
        harness.cleanup()
        self.assertFalse(resources)
        self.assertFalse(harness.cleanup_errors)
        self.assertFalse(harness.work.exists())
        self.assertEqual(sum(args[1] == "rm" for args in calls), 4)

    def test_cleanup_never_removes_a_resource_with_wrong_owner(self):
        harness, resources, calls = self.fake_harness()
        harness.output = None
        resources[("container", harness.name)] = "another-run"
        harness.cleanup()
        self.assertIn(("container", harness.name), resources)
        self.assertTrue(harness.cleanup_errors)
        self.assertFalse(any(args[1] == "rm" for args in calls))

    def test_mutations_check_owned_target_and_runtime_marker(self):
        harness, resources, calls = self.fake_harness()
        harness.container = harness.name
        resources[("container", harness.name)] = "another-run"
        with self.assertRaises(AssertionError): harness.target("touch", "/unauthorized")
        self.assertFalse(any(args[0] == "exec" for args in calls))
        harness.cleanup()

    def test_target_process_inspection_keeps_private_pid_namespace(self):
        harness, _, _ = self.fake_harness()

        class TargetLaunch(Exception):
            pass

        def docker(*args, **kwargs):
            if args[0] == "run":
                raise TargetLaunch()
            return ""

        try:
            with mock.patch.object(harness, "docker", side_effect=docker) as execute:
                with self.assertRaises(TargetLaunch):
                    harness.start(harness.work / "webserver")
                target = execute.call_args.args
            self.assertEqual(target[target.index("--name") + 1], harness.name)
            self.assertIn("--cap-add=SYS_PTRACE", target)
            self.assertFalse(any(arg in ("--privileged", "--pid") or arg.startswith("--pid=") for arg in target))
        finally:
            harness.cleanup()

    def test_reports_distinguish_unexecuted_from_passed_and_redact(self):
        harness, resources, calls = self.fake_harness()
        harness.records = [{"scenario": "root-upgrade", "status": "failed", "error": harness.password}]
        harness.cleanup()
        harness.report()
        report = json.loads((harness.output / "report.json").read_text())
        self.assertNotIn(harness.password, json.dumps(report))
        self.assertEqual(report["scenarios"][0]["error"], "[REDACTED]")
        self.assertTrue(all(record["status"] == "not_executed" for record in report["scenarios"][1:]))
        self.assertEqual({record["scenario"] for record in report["scenarios"]}, set(run.SCENARIOS))

    def test_image_pins_are_immutable_and_matrix_has_no_target_input(self):
        self.assertEqual(set(run.IMAGES), {"debian12", "debian13", "ubuntu2204", "ubuntu2404", "ubuntu2604"})
        for image in run.IMAGES.values():
            self.assertRegex(image, r"^(debian|ubuntu):[a-z0-9.-]+@sha256:[a-f0-9]{64}$")
        workflow = (run.REPO / ".github/workflows/ci.yml").read_text()
        job = workflow.split("  ssh-apt:\n", 1)[1].split("  ci-required:\n", 1)[0]
        self.assertIn("distro: [debian13, ubuntu2404]", job)
        self.assertNotIn("secrets.", job)
        self.assertNotIn("pull_request_target", workflow)
        self.assertIn("SSH_APT_RESULT: ${{ needs.ssh-apt.result }}", workflow)

    def test_docker_context_cannot_redirect_to_remote_daemon(self):
        with mock.patch.dict(os.environ, {"DOCKER_HOST": "ssh://production.example", "DOCKER_CONTEXT": "production"}), mock.patch.object(run, "command", return_value="ok") as execute:
            harness, _, _ = self.fake_harness()
            run.Harness.docker(harness, "version")
            self.assertEqual(execute.call_args.args[0][:2], ["docker", "--host=unix:///var/run/docker.sock"])
            harness.cleanup()

    def test_missing_vm_prerequisites_are_not_reported_as_success(self):
        spec = importlib.util.spec_from_file_location("vm_reboot", run.HERE / "vm-reboot.py")
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        with tempfile.TemporaryDirectory() as output, mock.patch("sys.argv", ["vm-reboot.py", "--check-prerequisites", "--output", output]), mock.patch.object(vm.shutil, "which", return_value=None), mock.patch.object(vm.subprocess, "Popen") as launch:
            self.assertEqual(vm.main(), 2)
            report = json.loads(Path(output, "report.json").read_text())
            self.assertEqual(report["status"], "not_executed")
            launch.assert_not_called()

    def test_cloud_image_hash_mismatch_cannot_start_vm(self):
        spec = importlib.util.spec_from_file_location("vm_reboot_hash", run.HERE / "vm-reboot.py")
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        with tempfile.TemporaryDirectory() as output:
            image = Path(output, "cloud.qcow2"); image.write_bytes(b"fixture")
            with mock.patch("sys.argv", ["vm-reboot.py", "--image", str(image), "--sha256", "0"*64, "--output", output]), mock.patch.object(vm.shutil, "which", return_value="available"), mock.patch.object(vm.subprocess, "Popen") as launch:
                self.assertEqual(vm.main(), 1)
                self.assertEqual(json.loads(Path(output, "report.json").read_text())["status"], "failed")
                launch.assert_not_called()


if __name__ == "__main__": unittest.main()
