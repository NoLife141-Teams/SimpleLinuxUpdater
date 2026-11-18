import json
import tempfile
import unittest
from pathlib import Path
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
                patch.object(operational, "docker", side_effect=RuntimeError("unavailable")) as docker:
            code, report = self.invoke(Path(directory))
        self.assertEqual(code, 2)
        self.assertEqual(report["status"], "not_executed")
        self.assertEqual(report["docker_upgrade"]["status"], "not_executed")
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")
        docker.assert_called_once_with("version", "--format", "{{.Server.Os}}/{{.Server.Arch}}", timeout=15)

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
        self.assertEqual(report["controlled_reboot_vm"]["status"], "not_executed")
        reboot.assert_not_called()
        cleanup.assert_called_once()
