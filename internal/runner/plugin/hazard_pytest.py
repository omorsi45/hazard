"""Hazard pytest plugin.

Runs exactly the node IDs listed, one per line, in the file named by the
HAZARD_SELECTION environment variable. Without that variable (a full run) it
does nothing.

A selected node ID that matches no collected item means the suite changed since
the selection was made, so the selection cannot be trusted. The unmatched IDs
are written to the file named by HAZARD_MISSING_OUT and the run exits with code
4 before any test executes.
"""

import os

import pytest


def _read_selection(path):
    with open(path, "r", encoding="utf-8", newline="\n") as fh:
        return {line.rstrip("\n") for line in fh if line.rstrip("\n")}


def pytest_collection_modifyitems(config, items):
    path = os.environ.get("HAZARD_SELECTION")
    if not path:
        return
    keep = _read_selection(path)
    found = {item.nodeid for item in items}
    missing = sorted(keep - found)
    if missing:
        out = os.environ.get("HAZARD_MISSING_OUT")
        if out:
            with open(out, "w", encoding="utf-8", newline="\n") as fh:
                fh.write("\n".join(missing) + "\n")
        pytest.exit(
            "hazard: %d selected test id(s) were not collected" % len(missing),
            returncode=4,
        )
    selected, deselected = [], []
    for item in items:
        (selected if item.nodeid in keep else deselected).append(item)
    if deselected:
        config.hook.pytest_deselected(items=deselected)
    items[:] = selected
