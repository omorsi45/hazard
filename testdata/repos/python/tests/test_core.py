from app import core


def test_add():
    assert core.add(1, 2) == 3


def test_base():
    assert core.base() == 10


class TestCoreClass:
    def test_add_negative(self):
        assert core.add(-1, -2) == -3
