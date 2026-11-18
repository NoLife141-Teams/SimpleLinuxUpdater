import io
import json
import subprocess
import tarfile
import tempfile
import unittest
import urllib.request
import urllib.response
from contextlib import contextmanager
from email.message import Message
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import operational


class OperationalQualificationTest(unittest.TestCase):
    def invoke(self, output):
        args = ["operational.py", "--candidate-source", str(output),
                "--candidate-sha", "a" * 40, "--output", str(output)]
        with patch("sys.argv", args), patch.object(operational.signal, "signal"):
            code = operational.main()
        return code, json.loads((output / "report.json").read_text())

    def test_docker_timeout_redacts_fixture_arguments_and_output(self):
        args = ("run", "--pull", "never", "python3", "-c", "private notification body")
        timeout = subprocess.TimeoutExpired((*operational.DOCKER, *args), 2, output=b"private output")
        with patch.object(operational.subprocess, "run", side_effect=timeout):
            with self.assertRaises(RuntimeError) as reported:
                operational.docker(*args, timeout=2)
        diagnostic = str(reported.exception)
        self.assertIn("timed out after 2 seconds", diagnostic)
        self.assertNotIn("private notification body", diagnostic)
        self.assertNotIn("private output", diagnostic)

    def test_unavailable_docker_is_reported_without_attempting_scenarios(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational.shutil, "which", return_value="/usr/bin/tool"), \
                patch.object(operational, "docker", side_effect=RuntimeError("unavailable")):
            output = Path(directory)
            proof = output / "vm-report.json"
            proof.write_text('{"status": "passed"}')
            code, report = self.invoke(output)
            self.assertFalse(proof.exists(), "an unexecuted retry must not retain an earlier VM pass")
        self.assertEqual(code, 2)
        self.assertEqual(report["status"], "not_executed")
        self.assertEqual(report["docker_upgrade"]["status"], "not_executed")
        self.assertEqual(report["docker_rollback"]["status"], "not_executed")
        self.assertEqual(report["maintenance_batches"]["status"], "not_executed")
        self.assertEqual(report["candidate_backup_restore"]["status"], "not_executed")
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")

    def test_batch_gate_failure_keeps_archive_result_and_marks_later_checks_unexecuted(self):
        backup = operational.PublishedBackup("published", {"username": "qualification-admin"}, b"archive", "phrase", "host key")
        upgraded = ("upgraded-container", object(), backup)

        def run_upgrade(qualification):
            qualification.report["docker_upgrade"]["separate_volume_restore"] = "passed"
            return upgraded

        def fail_batch(qualification, *args):
            self.assertEqual(args, (*upgraded[:2], backup.credentials))
            qualification.report["maintenance_batches"]["history_after_restart"] = "passed"
            raise AssertionError("retry admission was not unique")

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational.shutil, "which", return_value="/usr/bin/tool"), \
                patch.object(operational, "docker", return_value="linux/amd64"), \
                patch.object(operational.Qualification, "build_candidate"), \
                patch.object(operational.Qualification, "docker_upgrade", run_upgrade), \
                patch.object(operational.Qualification, "docker_rollback"), \
                patch.object(operational.Qualification, "qualify_maintenance_batches", fail_batch), \
                patch.object(operational.Qualification, "controlled_reboot") as reboot, \
                patch.object(operational.Qualification, "cleanup", return_value=True):
            code, report = self.invoke(Path(directory))
        self.assertEqual(code, 1)
        self.assertEqual(report["reason"], "retry admission was not unique")
        self.assertEqual(report["docker_upgrade"]["status"], "passed")
        self.assertEqual(report["docker_upgrade"]["separate_volume_restore"], "passed")
        self.assertEqual(report["docker_rollback"]["status"], "passed")
        self.assertEqual(report["maintenance_batches"]["status"], "failed")
        self.assertEqual(report["maintenance_batches"]["history_after_restart"], "passed")
        self.assertEqual(report["maintenance_batches"]["interrupted_job_recovery"], "not_executed")
        self.assertEqual(report["candidate_backup_restore"]["status"], "not_executed")
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")
        reboot.assert_not_called()

    def test_rollback_failure_keeps_migration_passed_and_cleans_up(self):
        volumes = {"other-run-volume": "other owner"}
        backup = operational.PublishedBackup("published", {}, b"archive", "phrase", "host key")

        def docker(*args, **kwargs):
            if args[:1] == ("version",):
                return "linux/amd64"
            if args[:2] == ("volume", "ls"):
                label = args[args.index("--filter") + 1].removeprefix("label=")
                return "\n".join(name for name, owner in volumes.items() if owner == label)
            if args[:2] == ("volume", "rm"):
                del volumes[args[2]]
                return ""
            if args[1] == "ls":
                return ""
            self.fail(f"Unexpected Docker command: {args[:2]}")

        def run_upgrade(qualification):
            volumes[qualification.prefix + "-candidate-data"] = qualification.label
            qualification.report["docker_upgrade"]["copied_volume_restart"] = "passed"
            qualification.report["docker_upgrade"]["separate_volume_restore"] = "passed"
            return "upgraded-container", object(), backup

        def fail_rollback(qualification, _backup):
            volumes[qualification.prefix + "-rollback-data"] = qualification.label
            raise AssertionError("fixture SSH host-key trust was not preserved")

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational.shutil, "which", return_value="/usr/bin/tool"), \
                patch.object(operational, "docker", docker), \
                patch.object(operational.Qualification, "build_candidate"), \
                patch.object(operational.Qualification, "docker_upgrade", run_upgrade), \
                patch.object(operational.Qualification, "docker_rollback", fail_rollback):
            code, report = self.invoke(Path(directory))
        self.assertEqual(code, 1)
        self.assertEqual(report["status"], "failed")
        self.assertEqual(report["docker_upgrade"]["status"], "passed")
        self.assertEqual(report["docker_rollback"]["status"], "failed")
        self.assertEqual(report["maintenance_batches"]["status"], "not_executed")
        self.assertEqual(report["reason"], "fixture SSH host-key trust was not preserved")
        self.assertEqual(report["cleanup"]["status"], "passed")
        self.assertEqual(volumes, {"other-run-volume": "other owner"})

    def test_expiring_browser_cookie_during_restore_does_not_prove_session_invalidation(self):
        class RestoreTransport(urllib.request.HTTPHandler):
            handler_order = 100
            initialized = False

            def http_open(self, request):
                headers = Message()
                path = request.selector
                if path == "/api/auth/status":
                    result = {"setup_required": not self.initialized,
                              "authenticated": request.get_header("Cookie") == "fixture-initial=session"}
                elif path == "/api/auth/setup":
                    self.initialized = True
                    headers["Set-Cookie"] = "fixture-initial=session; Path=/"
                    result = {}
                elif path == "/api/backup/verify":
                    result = {"restore_ready": True}
                elif path == "/api/backup/restore":
                    headers["Set-Cookie"] = "fixture-initial=; Path=/; Max-Age=0"
                    result = {}
                else:
                    raise AssertionError(f"Unexpected request {path}")
                response = urllib.response.addinfourl(io.BytesIO(json.dumps(result).encode()), headers, request.full_url, 200)
                response.msg = "OK"
                return response

        transport = RestoreTransport()
        api_type = operational.API

        def api_with_fixture(**kwargs):
            api = api_type(**kwargs)
            api.client.add_handler(transport)
            return api

        qualification = operational.Qualification(Path("."), Path("."), Path("."), "a" * 40)
        backup = operational.PublishedBackup("published", {}, b"archive", "phrase", "host key")
        with patch.object(operational, "API", api_with_fixture), \
                patch.object(qualification, "start_app", return_value=api_with_fixture()):
            with self.assertRaisesRegex(AssertionError, "restore retained the previous session"):
                qualification.restore_archive("rollback", backup.image, "fresh-volume", backup, "rollback-admin")

    def test_cleanup_fails_when_a_labeled_volume_remains_after_removal(self):
        qualification = operational.Qualification(Path("."), Path("."), Path("."), "a" * 40)

        def docker(*args):
            if args[:2] == ("volume", "ls"):
                self.assertIn("label=" + qualification.label, args)
                return qualification.prefix + "-candidate-restore-data"
            return ""

        with patch.object(operational, "docker", docker):
            self.assertFalse(qualification.cleanup())
        self.assertEqual(qualification.report["cleanup"]["status"], "failed")
        self.assertIn("Owned qualification volume resources remain", qualification.report["cleanup"]["errors"])

    def test_failed_upgrade_reports_vm_not_executed_and_cleans_up(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational.shutil, "which", return_value="/usr/bin/tool"), \
                patch.object(operational, "docker", return_value="linux/amd64"), \
                patch.object(operational.Qualification, "build_candidate"), \
                patch.object(operational.Qualification, "docker_upgrade", side_effect=RuntimeError("copy failed")), \
                patch.object(operational.Qualification, "controlled_reboot") as reboot, \
                patch.object(operational.Qualification, "cleanup", return_value=True) as cleanup:
            code, report = self.invoke(Path(directory))
        self.assertEqual(code, 1)
        self.assertEqual(report["status"], "failed")
        self.assertEqual(report["docker_upgrade"]["status"], "failed")
        self.assertEqual(report["docker_rollback"]["status"], "not_executed")
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")
        reboot.assert_not_called()
        cleanup.assert_called_once()

    def test_cleanup_failure_cannot_publish_successful_terminal_report(self):
        def fail_cleanup():
            raise PermissionError("private filesystem detail")

        with tempfile.TemporaryDirectory() as report_directory, tempfile.TemporaryDirectory() as work_directory, \
                patch.object(operational.tempfile, "TemporaryDirectory",
                             return_value=SimpleNamespace(name=work_directory, cleanup=fail_cleanup)), \
                patch.object(operational.Qualification, "run", return_value=0):
            code, report = self.invoke(Path(report_directory))
        self.assertEqual(code, 1)
        self.assertEqual(report["status"], "failed")
        self.assertEqual(report["cleanup"]["status"], "failed")
        self.assertNotIn("private filesystem detail", json.dumps(report))

    def test_unsuccessful_vm_process_cannot_report_passed(self):
        @contextmanager
        def archive_with_failed_cleanup(_):
            data = json.dumps({"status": "passed", "cleanup": {"status": "failed"}}).encode()
            member = tarfile.TarInfo("report.json")
            member.size = len(data)
            with io.BytesIO() as stream:
                with tarfile.open(fileobj=stream, mode="w") as archive:
                    archive.addfile(member, io.BytesIO(data))
                stream.seek(0)
                with tarfile.open(fileobj=stream) as archive:
                    yield archive

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational, "container_archive", archive_with_failed_cleanup):
            output = Path(directory)
            qualification = operational.Qualification(output, output, output, "a" * 40)
            with self.assertRaises(RuntimeError):
                qualification.collect_vm_evidence("fixture", "1")
            evidence = json.loads((output / "vm-report.json").read_text())
        self.assertEqual(evidence["status"], "failed")
        self.assertEqual(qualification.report["controlled_reboot_vm"]["status"], "failed")

    def test_host_api_does_not_fetch_redirect_target(self):
        class FixtureTransport(urllib.request.HTTPHandler):
            handler_order = 100
            requests = []

            def http_open(self, request):
                self.requests.append(request.full_url)
                headers = Message()
                headers["Location"] = "http://127.0.0.1:9999/api/auth/status"
                response = urllib.response.addinfourl(io.BytesIO(b""), headers, request.full_url, 302)
                response.msg = "Found"
                return response

        api = operational.API(follow_redirects=False)
        transport = FixtureTransport()
        api.client.add_handler(transport)
        with self.assertRaisesRegex(AssertionError, "GET /api/auth/status: HTTP 302"):
            api.request("GET", "/api/auth/status")
        self.assertEqual(transport.requests, [api.base + "/api/auth/status"])
