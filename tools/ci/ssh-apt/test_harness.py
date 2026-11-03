import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import run


class HarnessSafetyTests(unittest.TestCase):
    def test_api_request_uses_the_callers_remaining_timeout(self):
        api = run.API(30000)
        response = mock.Mock(code=200)
        response.read.return_value = b'{}'
        with mock.patch.object(api.client, 'open', return_value=response) as execute:
            self.assertEqual(api.request('GET', '/api/auth/status', timeout=.25), {})
        self.assertEqual(execute.call_args.kwargs['timeout'], .25)

    def test_vm_startup_retries_connection_errors_and_actual_maintenance_status(self):
        spec = importlib.util.spec_from_file_location('vm_reboot_readiness', run.HERE / 'vm-reboot.py')
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        api = mock.Mock()
        api.request.side_effect = [OSError('not listening'),
            AssertionError('GET /api/auth/status: HTTP 503: {"error":"maintenance mode active"}'), {}]
        clock = [0.0]
        with mock.patch.object(vm.time, 'monotonic', side_effect=lambda: clock[0]), \
             mock.patch.object(vm.time, 'sleep', side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
            vm.wait_for_app(api)
        self.assertEqual(api.request.call_count, 3)
        for call in api.request.call_args_list:
            self.assertEqual(call.args, ('GET', '/api/auth/status'))
            self.assertEqual(call.kwargs, {'timeout': 20})
        self.assertAlmostEqual(clock[0], .2)

    def test_vm_startup_permanent_errors_are_not_retried(self):
        spec = importlib.util.spec_from_file_location('vm_reboot_permanent', run.HERE / 'vm-reboot.py')
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        errors = [AssertionError('GET /api/auth/status: HTTP ' + str(code) + ': {}') for code in (400, 401, 403, 500)]
        errors += [AssertionError('GET /api/auth/status: HTTP 500: {"error":"HTTP 503:"}'),
                   AssertionError('unrelated HTTP 503: assertion'), KeyboardInterrupt()]
        for error in errors:
            with self.subTest(error=error):
                api = mock.Mock()
                api.request.side_effect = error
                with mock.patch.object(vm.time, 'sleep') as sleep, self.assertRaises(type(error)):
                    vm.wait_for_app(api)
                self.assertEqual(api.request.call_count, 1)
                sleep.assert_not_called()

    def test_vm_startup_timeout_stops_without_advancing_to_setup(self):
        spec = importlib.util.spec_from_file_location('vm_reboot_timeout', run.HERE / 'vm-reboot.py')
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        clock = [0.0]
        api = mock.Mock()
        timeouts = []
        def request(*args, **kwargs):
            self.assertEqual(args, ('GET', '/api/auth/status'))
            self.assertEqual(kwargs['timeout'], min(20, 30 - clock[0]))
            timeouts.append(kwargs['timeout'])
            raise AssertionError('GET /api/auth/status: HTTP 503: {}')
        api.request.side_effect = request
        with mock.patch.object(vm.time, 'monotonic', side_effect=lambda: clock[0]), \
             mock.patch.object(vm.time, 'sleep', side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)), \
             self.assertRaisesRegex(TimeoutError, 'not ready within 30 seconds'):
            vm.wait_for_app(api)
        self.assertEqual(clock[0], 30)
        self.assertLess(timeouts[-1], 1)

    def test_container_startup_retries_real_maintenance_response_before_setup(self):
        harness = object.__new__(run.Harness)
        harness.app_name = "isolated-app"
        harness.api = run.API(30000)
        harness.docker = mock.Mock(return_value="true")
        maintenance = mock.Mock(code=503)
        maintenance.read.return_value = b'{"maintenance":true,"error":"maintenance mode active"}'
        ready = mock.Mock(code=200)
        ready.read.return_value = b'{}'
        clock = [0.0]
        with mock.patch.object(harness.api.client, 'open', side_effect=[OSError('not listening'), maintenance, ready]) as request, \
             mock.patch.object(run.time, 'monotonic', side_effect=lambda: clock[0]), \
             mock.patch.object(run.time, 'sleep', side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)):
            harness.wait_for_app()
        self.assertEqual(request.call_count, 3)
        self.assertEqual(harness.docker.call_count, 3)
        self.assertAlmostEqual(clock[0], .2)
        for call in request.call_args_list:
            self.assertEqual(call.args[0].get_method(), 'GET')
            self.assertTrue(call.args[0].full_url.endswith('/api/auth/status'))
        for call in harness.docker.call_args_list:
            self.assertEqual(call.args, ('inspect', '--format', '{{.State.Running}}', harness.app_name))
            self.assertEqual(call.kwargs, {'timeout': 20})

    def test_container_startup_fails_when_application_exits(self):
        harness = object.__new__(run.Harness)
        harness.app_name = "isolated-app"
        harness.api = mock.Mock()
        harness.docker = mock.Mock(return_value="false")
        with self.assertRaisesRegex(AssertionError, 'exited before readiness'):
            harness.wait_for_app()
        harness.api.request.assert_not_called()

    def test_container_startup_inspection_and_http_share_deadline(self):
        harness = object.__new__(run.Harness)
        harness.app_name = "isolated-app"
        harness.api = mock.Mock()
        clock = [0.0]
        timeouts = []

        def docker(*args, **kwargs):
            self.assertEqual(kwargs['timeout'], min(20, 30 - clock[0]))
            clock[0] += min(5, 30 - clock[0])
            return "true"

        def request(*args, **kwargs):
            self.assertEqual(args, ('GET', '/api/auth/status'))
            self.assertEqual(kwargs['timeout'], min(20, 30 - clock[0]))
            timeouts.append(kwargs['timeout'])
            raise AssertionError('GET /api/auth/status: HTTP 503: {}')

        harness.docker = mock.Mock(side_effect=docker)
        harness.api.request.side_effect = request
        with mock.patch.object(run.time, 'monotonic', side_effect=lambda: clock[0]), \
             mock.patch.object(run.time, 'sleep', side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)), \
             self.assertRaisesRegex(TimeoutError, 'not ready within 30 seconds'):
            harness.wait_for_app()
        self.assertEqual(clock[0], 30)
        self.assertLess(timeouts[-1], timeouts[0])
        self.assertTrue(all(call.args == ('GET', '/api/auth/status') for call in harness.api.request.call_args_list))

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

    def test_workflows_collect_only_visible_redacted_evidence_files(self):
        for filename in ("ci.yml", "ssh-apt-extended.yml"):
            with self.subTest(filename=filename):
                workflow = (run.REPO / ".github" / "workflows" / filename).read_text()
                job = workflow.split("  ssh-apt:\n", 1)[1].split("  ci-required:\n", 1)[0] if filename == "ci.yml" else workflow
                self.assertIn("--output ssh-apt-evidence", job)
                self.assertNotIn("include-hidden-files: true", job)
                lines = job.splitlines()
                index = next(i for i, line in enumerate(lines) if line.strip() == "path: |")
                indent = len(lines[index]) - len(lines[index].lstrip())
                files = []
                for line in lines[index + 1:]:
                    if line.strip() and len(line) - len(line.lstrip()) <= indent:
                        break
                    if line.strip():
                        files.append(line.strip())
                self.assertEqual(files, ["ssh-apt-evidence/report.json", "ssh-apt-evidence/app.log",
                                         "deferrals-evidence/report.json", "deferrals-evidence/negative.json", "deferrals-evidence/positive.json"])

    def test_collected_evidence_never_copies_private_runtime_files(self):
        harness, resources, _ = self.fake_harness()
        resources[("container", harness.app_name)] = harness.owner
        key = "-----BEGIN PRIVATE KEY-----\nfixture-only-canary\n-----END PRIVATE KEY-----\n"
        harness.redactions.append(key)
        for filename in ("servers.db", "config.json", "known_hosts", "client"):
            (harness.work / filename).write_text(harness.password + key)
        execute = harness.docker

        def docker(*args, **kwargs):
            if args[0] == "logs":
                return "safe diagnostic " + harness.password + " " + key
            return execute(*args, **kwargs)

        harness.docker = docker
        harness.records = [{"scenario": "failure", "status": "failed", "error": harness.password + " " + key}]
        writes = []
        write_text = Path.write_text

        def observe_write(path, text, *args, **kwargs):
            writes.append((path, text))
            return write_text(path, text, *args, **kwargs)

        with mock.patch.object(Path, "write_text", new=observe_write):
            harness.cleanup()
            harness.report()
        self.assertFalse(harness.cleanup_errors)
        self.assertFalse(harness.work.exists())
        self.assertEqual({path for path, _ in writes}, {harness.output / "app.log", harness.output / "report.json"})
        for path, text in writes:
            with self.subTest(path=path):
                self.assertNotIn(harness.password, text)
                self.assertNotIn(key, text)
                self.assertNotIn(json.dumps(key)[1:-1], text)
        self.assertEqual({p.name for p in harness.output.iterdir()}, {"app.log", "report.json"})
        for filename in ("app.log", "report.json"):
            text = (harness.output / filename).read_text()
            self.assertIn("[REDACTED]", text)
            self.assertNotIn(harness.password, text)
            self.assertNotIn(key, text)

    def test_log_redaction_precedes_tail_limit(self):
        harness, resources, _ = self.fake_harness()
        resources[("container", harness.app_name)] = harness.owner
        padding = "x" * (200000 - len(harness.password) // 2)
        execute = harness.docker

        def docker(*args, **kwargs):
            if args[0] == "logs":
                return harness.password + padding
            return execute(*args, **kwargs)

        harness.docker = docker
        harness.cleanup()
        self.assertFalse(harness.cleanup_errors)
        text = (harness.output / "app.log").read_text()
        self.assertEqual(text, "[REDACTED]" + padding)
        self.assertLessEqual(len(text), 200000)

    def test_primary_credential_is_masked_without_optional_redactions(self):
        harness, _, _ = self.fake_harness()
        self.assertEqual(harness.redactions, [])
        harness.password = 'fixture-only-primary"\\\ncanary'
        text = "plain=" + harness.password + "; encoded=" + json.dumps(harness.password)[1:-1]
        self.assertEqual(harness.redact_text(text), "plain=[REDACTED]; encoded=[REDACTED]")

    def test_report_redacts_nested_multiline_values_before_json_encoding(self):
        harness, _, _ = self.fake_harness()
        key = "-----BEGIN PRIVATE KEY-----\nfixture-only-quote\"-slash\\-canary-" + harness.password + "\n-----END PRIVATE KEY-----\n"
        harness.redactions.append(key)
        harness.records = [{"scenario": "failure", "status": "failed", "error": key,
                            "nested": {"detail": [harness.password, (key,)]},
                            "encoded": json.dumps({"detail": key})}]
        harness.cleanup()
        harness.report()
        report = json.loads((harness.output / "report.json").read_text())
        self.assertEqual(report["scenarios"][0]["error"], "[REDACTED]")
        self.assertEqual(report["scenarios"][0]["nested"], {"detail": ["[REDACTED]", ["[REDACTED]"]]})
        self.assertEqual(json.loads(report["scenarios"][0]["encoded"]), {"detail": "[REDACTED]"})

    def fake_harness(self):
        output = tempfile.TemporaryDirectory()
        self.addCleanup(output.cleanup)
        harness = run.Harness("debian13", Path(output.name))
        self.addCleanup(shutil.rmtree, harness.work, ignore_errors=True)
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

    def test_cleanup_removes_resources_when_evidence_collection_fails(self):
        for failure in ("output-directory", "output-log", "docker-logs-timeout"):
            with self.subTest(failure=failure):
                harness, resources, calls = self.fake_harness()
                for kind, name in (("container", harness.app_name), ("container", harness.name),
                                   ("network", harness.network), ("image", harness.image)):
                    resources[(kind, name)] = harness.owner
                if failure == "output-directory":
                    harness.output = harness.output / "existing-file"
                    harness.output.touch()
                elif failure == "output-log":
                    (harness.output / "app.log").mkdir()
                docker = harness.docker

                def execute(*args, **kwargs):
                    if args[0] == "logs":
                        if failure == "docker-logs-timeout":
                            raise subprocess.TimeoutExpired("docker logs", 60)
                        return "safe application log"
                    return docker(*args, **kwargs)

                harness.docker = execute
                harness.cleanup()
                self.assertFalse(resources)
                self.assertFalse(harness.work.exists())
                self.assertEqual(sum(args[1] == "rm" for args in calls), 4)
                self.assertEqual(len(harness.cleanup_errors), 1)
                if failure != "output-directory":
                    harness.report()
                    report = json.loads((harness.output / "report.json").read_text())
                    self.assertEqual(report["cleanup"]["status"], "failed")
                    self.assertEqual(report["cleanup"]["errors"], harness.cleanup_errors)

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

    def test_vm_cleanup_removes_private_files_when_evidence_fails(self):
        spec = importlib.util.spec_from_file_location("vm_reboot_cleanup", run.HERE / "vm-reboot.py")
        vm = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(vm)
        for failure in (None, "output-log", "vm-log-read", "startup-timeout"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as root:
                root = Path(root)
                image = root / "cloud.qcow2"
                image.write_bytes(b"owned cloud image fixture")
                work = root / "private"
                work.mkdir()
                output = root / "evidence"
                output.mkdir()
                if failure == "output-log": (output / "vm.log").mkdir()
                owner = "a" * 32
                app_name = "slu-reboot-app-" + owner
                resources = set()
                clock = [0.0]
                process = mock.Mock()
                process.poll.return_value = None

                def execute(args, **kwargs):
                    if args[0] == "ssh-keygen":
                        (work / "client").write_text("generated fixture private key")
                        (work / "client.pub").write_text("ssh-ed25519 fixture")
                    elif args[0] == "cloud-localds":
                        (work / "seed.img").write_text("private cloud-init seed")
                    elif args[0] == "qemu-img":
                        (work / "overlay.qcow2").write_text("private VM overlay")
                    elif args[0] == "ssh":
                        # A first SSH connection otherwise prefixes stdout with
                        # its accepted-host warning, breaking marker validation.
                        if "LogLevel=ERROR" not in args:
                            return "Warning: Permanently added fixture host.\n" + owner
                        return {"cat /var/lib/slu-fixture-owner": owner,
                                "systemctl is-active ssh.service": "active",
                                "cut -d' ' -f1 /proc/uptime": "120"}[args[-1]]
                    elif args[0] == "docker":
                        self.assertEqual(args[:2], run.DOCKER)
                        if args[2] == "version": return "amd64"
                        if args[2] == "run": resources.add(app_name)
                        elif args[2:4] == ["container", "inspect"]:
                            self.assertIn(args[-1], resources)
                            return owner
                        elif args[2] == "rm": resources.remove(args[-1])
                    return ""

                def request(method, path, data=None, **kwargs):
                    if failure == "startup-timeout" and path == "/api/auth/status":
                        raise AssertionError('GET /api/auth/status: HTTP 503: {}')
                    if path == "/api/auth/setup": raise RuntimeError("injected setup failure")
                    return {}

                read_text = Path.read_text

                def read(path, *args, **kwargs):
                    if failure == "vm-log-read" and path == work / "vm.log":
                        raise PermissionError("injected VM log read failure")
                    return read_text(path, *args, **kwargs)

                with mock.patch("sys.argv", ["vm-reboot.py", "--image", str(image), "--sha256", hashlib.sha256(image.read_bytes()).hexdigest(), "--output", str(output)]), \
                        mock.patch.object(vm.shutil, "which", return_value="available"), \
                        mock.patch.object(vm.tempfile, "mkdtemp", return_value=str(work)), \
                        mock.patch.object(vm.uuid, "uuid4", return_value=mock.Mock(hex=owner)), \
                        mock.patch.object(vm, "command", side_effect=execute), \
                        mock.patch.object(vm, "loopback_port", return_value=30000), \
                        mock.patch.object(vm, "copy_assets"), \
                        mock.patch.object(vm, "API") as api, \
                        mock.patch.object(vm.subprocess, "Popen", return_value=process), \
                        mock.patch.object(vm.signal, "signal"), \
                        mock.patch.object(vm.time, "monotonic", side_effect=lambda: clock[0]), \
                        mock.patch.object(vm.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)), \
                        mock.patch.object(Path, "read_text", new=read):
                    api.return_value.request.side_effect = request
                    self.assertEqual(vm.main(), 1)
                process.terminate.assert_called_once()
                process.wait.assert_called_once_with(timeout=10)
                self.assertFalse(resources)
                self.assertFalse(work.exists())
                self.assertEqual(image.read_bytes(), b"owned cloud image fixture")
                report = json.loads((output / "report.json").read_text())
                self.assertEqual(report["status"], "failed")
                self.assertEqual(report["error"], "application startup was not ready within 30 seconds" if failure == "startup-timeout" else "injected setup failure")
                evidence_error = failure in ("output-log", "vm-log-read")
                self.assertEqual(report["cleanup"]["status"], "failed" if evidence_error else "passed")
                self.assertEqual(len(report["cleanup"]["errors"]), 1 if evidence_error else 0)


if __name__ == "__main__": unittest.main()
