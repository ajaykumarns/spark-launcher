"""Process locks that the operating system releases if a command exits."""
import fcntl
import os
from contextlib import contextmanager
from pathlib import Path

from .catalog import SparkError


@contextmanager
def file_lock(path, message):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SparkError(message) from None
        yield
    finally:
        # Keep the inode. Removing it can let a third process bypass a waiter.
        os.close(fd)
