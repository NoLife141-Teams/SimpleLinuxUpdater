"""Real local children prove capture, inherited streams and failure propagation."""
import importlib.util
from pathlib import Path
import selectors
import subprocess
import sys
import unittest

POLICY = Path(__file__).with_name('publication-policy.py')
spec = importlib.util.spec_from_file_location('output_policy', POLICY)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


def wrapper(code):
    return f'''import importlib.util, sys
sys.path.insert(0, {str(POLICY.parent)!r})
spec = importlib.util.spec_from_file_location('policy', {str(POLICY)!r})
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
{code}
'''


class CommandOutputTests(unittest.TestCase):
    def test_capture_returns_stripped_response_without_printing_it(self):
        script = wrapper('''value = policy.capture_command(sys.executable, '-c', "print('  private-response  ')")
assert value == 'private-response'
print('capture-complete')''')
        result = subprocess.run([sys.executable, '-c', script], capture_output=True, text=True, check=True)
        self.assertEqual(result.stdout, 'capture-complete\n')
        self.assertEqual(result.stderr, '')

    def test_run_streams_both_outputs_before_child_is_allowed_to_exit(self):
        # The child cannot exit until both streams are observed. A capturing helper
        # deadlocks this handshake; select's timeout is only a deadlock guard.
        child = "import sys; print('child-out', flush=True); print('child-err', file=sys.stderr, flush=True); assert sys.stdin.readline() == 'continue\\n'"
        script = wrapper(f"policy.run_command(sys.executable, '-c', {child!r})")
        with subprocess.Popen([sys.executable, '-c', script], stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE) as process:
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ, b'child-out\n')
                    selector.register(process.stderr, selectors.EVENT_READ, b'child-err\n')
                    while selector.get_map():
                        events = selector.select(timeout=10)
                        self.assertTrue(events, 'child output was captured instead of streamed')
                        for key, _ in events:
                            self.assertEqual(key.fileobj.readline(), key.data)
                            selector.unregister(key.fileobj)
                self.assertIsNone(process.poll(), 'child must still be waiting for the handshake')
                process.stdin.write(b'continue\n')
                process.stdin.flush()
                self.assertEqual(process.wait(timeout=10), 0)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()

    def test_both_helpers_propagate_real_nonzero_exit(self):
        for helper in (policy.capture_command, policy.run_command):
            with self.subTest(helper=helper.__name__):
                with self.assertRaises(subprocess.CalledProcessError) as raised:
                    helper(sys.executable, '-c', 'import sys; sys.exit(23)')
                self.assertEqual(raised.exception.returncode, 23)
