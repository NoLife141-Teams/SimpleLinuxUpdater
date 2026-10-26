#!/usr/bin/env python3
"""Exercise the real recovery terminal without putting passwords in argv."""
import os
import pty
import select
import signal
import sqlite3
import subprocess
import sys
import termios
import time

binary, database = sys.argv[1:]
secret = b"TTY-Recovery-Password123!"


def read_until(master, output, marker):
    end = time.monotonic() + 15
    while marker not in output:
        if time.monotonic() >= end:
            raise AssertionError("recovery terminal prompt timed out")
        if select.select([master], [], [], .1)[0]:
            output.extend(os.read(master, 4096))


def await_hidden(slave):
    end = time.monotonic() + 3
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        assert time.monotonic() < end, "password echo was not disabled"
        time.sleep(.001)


for termination in (None, signal.SIGINT, signal.SIGTERM):
    master, slave = pty.openpty()
    original = termios.tcgetattr(slave)
    process = subprocess.Popen([binary, "-test.run=^TestRecoveryCommandProcessHelper$"],
                               stdin=slave, stdout=slave, stderr=slave, env=os.environ)
    output = bytearray()
    before = sqlite3.connect(database).execute("SELECT password_hash FROM auth_users").fetchone()[0]
    try:
        read_until(master, output, b"New password: ")
        await_hidden(slave)
        if termination is not None:
            os.write(master, secret)
            process.send_signal(termination)
            assert process.wait(timeout=10) == 130, "cancellation exit code"
            read_until(master, output, b"Recovery cancelled.")
            after = sqlite3.connect(database).execute("SELECT password_hash FROM auth_users").fetchone()[0]
            assert before == after, "interrupted recovery changed the account"
            os.write(master, b"\n")
            assert os.read(slave, 256) == b"\n", "cancelled password remained in terminal input"
        else:
            os.write(master, secret + b"\n")
            read_until(master, output, b"Confirm password: ")
            await_hidden(slave)
            os.write(master, secret + b"\n")
            assert process.wait(timeout=15) == 0, "terminal recovery failed"
            read_until(master, output, b"recovery committed.")
        assert secret not in output, "terminal output disclosed password"
        assert termios.tcgetattr(slave) == original, "terminal mode was not restored"
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        os.close(master)
        os.close(slave)
