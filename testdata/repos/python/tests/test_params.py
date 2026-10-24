import pytest


@pytest.mark.parametrize(
    "value",
    [
        pytest.param(1, id="with space"),
        pytest.param(2, id="slash/inside"),
        pytest.param(3, id="open[bracket"),
        pytest.param(4, id="dash-ed"),
        pytest.param(5, id="caf\u00e9"),
    ],
)
def test_value_is_positive(value):
    assert value > 0


class TestParamsInClass:
    @pytest.mark.parametrize("word", ["plain", "two words", "a-b/c"])
    def test_word_is_not_empty(self, word):
        assert word
