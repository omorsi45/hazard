import json
import math
import os
import subprocess
import sys

import pytest

from hazard_research import reliability

RESEARCH_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# The fixture: a comment line naming the population, the header, and pairs of
# two configurations. P0 has pairs in bins 0, 1, 5 and 9.
PAIRS_CSV = """# synthetic (mutation labels)
event,test,p,label,config
m1,go:p:TestA,0.0,0,P0
m1,go:p:TestB,0.05,0,P0
m2,go:p:TestA,0.15,1,P0
m2,"go:p:Test,B",0.5,0,P0
m3,go:p:TestA,0.95,1,P0
m3,go:p:TestB,1.0,0,P0
m1,go:p:TestA,0.31,1,P1
m2,go:p:TestA,0.39,0,P1
"""


@pytest.fixture
def pairs_file(tmp_path):
    path = tmp_path / "pairs.csv"
    path.write_text(PAIRS_CSV, encoding="utf-8", newline="\n")
    return path


@pytest.mark.parametrize(
    "p,want",
    [(0.0, 0), (0.0999, 0), (0.1, 1), (0.5, 5), (0.95, 9), (0.9999, 9), (1.0, 9)],
)
def test_bin_of_places_the_edges(p, want):
    assert reliability.bin_of(p) == want


@pytest.mark.parametrize("p", [-0.01, 1.01, float("nan"), float("inf")])
def test_bin_of_refuses_what_is_not_a_probability(p):
    with pytest.raises(ValueError):
        reliability.bin_of(p)


def test_read_pairs_skips_the_comment_and_keeps_quoted_commas(pairs_file):
    rows = reliability.read_pairs(str(pairs_file))
    assert len(rows) == 8
    assert rows[0] == {"event": "m1", "test": "go:p:TestA", "p": 0.0, "label": 0, "config": "P0"}
    assert rows[3]["test"] == "go:p:Test,B"
    assert rows[2]["label"] == 1


@pytest.mark.parametrize(
    "text",
    [
        "event,test,p,label\nm1,t,0.5,1\n",  # no config column
        "event,test,p,label,config\nm1,t,0.5,2,P0\n",  # label is 0 or 1
        "event,test,p,label,config\nm1,t,1.5,1,P0\n",  # not a probability
        "event,test,p,label,config\nm1,t,abc,1,P0\n",
    ],
)
def test_read_pairs_refuses_malformed_input(tmp_path, text):
    path = tmp_path / "bad.csv"
    path.write_text(text, encoding="utf-8")
    with pytest.raises(ValueError):
        reliability.read_pairs(str(path))


def test_reliability_counts_means_and_observed_rates(pairs_file):
    out = reliability.reliability(reliability.read_pairs(str(pairs_file)))
    assert list(out) == ["P0", "P1"]
    bins = out["P0"]
    assert len(bins) == 10
    want = {0: (2, 0.025, 0.0), 1: (1, 0.15, 1.0), 5: (1, 0.5, 0.0), 9: (2, 0.975, 0.5)}
    for index, (count, mean_p, observed) in want.items():
        b = bins[index]
        assert b["bin"] == index and b["count"] == count
        assert math.isclose(b["mean_p"], mean_p, abs_tol=1e-12)
        assert math.isclose(b["observed_rate"], observed, abs_tol=1e-12)
    assert math.isclose(bins[3]["lo"], 0.3) and math.isclose(bins[3]["hi"], 0.4)
    for index in (2, 3, 4, 6, 7, 8):
        assert bins[index]["count"] == 0
        assert bins[index]["mean_p"] is None and bins[index]["observed_rate"] is None
    # the other configuration is tabulated apart: 0.31 and 0.39 are both in bin 3
    assert out["P1"][3]["count"] == 2 and out["P1"][3]["observed_rate"] == 0.5
    assert out["P1"][0]["count"] == 0


def test_reliability_of_no_pairs_is_empty():
    assert reliability.reliability([]) == {}


def run_cli(*args):
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1", PYTHONIOENCODING="utf-8")
    return subprocess.run(
        [sys.executable, "-m", "hazard_research.reliability", *args],
        cwd=RESEARCH_DIR,
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
        check=False,
    )


def test_cli_prints_the_bins_as_json(pairs_file):
    proc = run_cli("--pairs", str(pairs_file))
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert sorted(out["configs"]) == ["P0", "P1"]
    assert out["configs"]["P0"][9]["count"] == 2
    assert out["configs"]["P0"][4]["mean_p"] is None


def test_cli_rejects_a_missing_file(tmp_path):
    proc = run_cli("--pairs", str(tmp_path / "nope.csv"))
    assert proc.returncode != 0
