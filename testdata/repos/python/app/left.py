"""Left arm of the fixture dependency diamond."""

from app import core


def value():
    return core.base() + 1
