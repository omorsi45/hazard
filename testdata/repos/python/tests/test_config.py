from pathlib import Path

DEFAULTS = Path(__file__).resolve().parent.parent / "config" / "defaults.yaml"


def test_default_port():
    # Plain file IO on purpose: the dependency on the yaml file has no import edge.
    with open(DEFAULTS, "r", encoding="utf-8") as fh:
        text = fh.read()
    assert "port: 8080" in text
