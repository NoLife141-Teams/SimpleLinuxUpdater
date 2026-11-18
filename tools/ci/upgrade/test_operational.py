import io
import json
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
                patch.object(operational.Qualification, "docker_rollback") as rollback, \
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
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")
        rollback.assert_called_once_with(backup)
        reboot.assert_not_called()

    def test_rollback_failure_keeps_migration_passed_and_cleans_up(self):
        def run_upgrade(qualification):
            qualification.report["docker_upgrade"]["copied_volume_restart"] = "passed"
            qualification.report["docker_upgrade"]["separate_volume_restore"] = "passed"
            return "upgraded-container", object(), backup

        backup = operational.PublishedBackup("published", {}, b"archive", "phrase", "host key")
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(operational.shutil, "which", return_value="/usr/bin/tool"), \
                patch.object(operational, "docker", return_value="linux/amd64"), \
                patch.object(operational.Qualification, "build_candidate"), \
                patch.object(operational.Qualification, "docker_upgrade", run_upgrade), \
                patch.object(operational.Qualification, "docker_rollback", side_effect=AssertionError("fixture SSH host-key trust was not preserved")), \
                patch.object(operational.Qualification, "qualify_maintenance_batches") as batches, \
                patch.object(operational.Qualification, "cleanup", return_value=True) as cleanup:
            code, report = self.invoke(Path(directory))
        self.assertEqual(code, 1)
        self.assertEqual(report["status"], "failed")
        self.assertEqual(report["docker_upgrade"]["status"], "passed")
        self.assertEqual(report["docker_rollback"]["status"], "failed")
        self.assertEqual(report["maintenance_batches"]["status"], "not_executed")
        self.assertEqual(report["reason"], "fixture SSH host-key trust was not preserved")
        batches.assert_not_called()
        cleanup.assert_called_once()

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
