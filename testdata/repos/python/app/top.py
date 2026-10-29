"""Apex of the fixture dependency diamond."""

from app import left, right


def total():
    return left.value() + right.value()
