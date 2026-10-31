import json
import math
import os
import subprocess
import sys

import pytest

from hazard_research import ltt_check

RESEARCH_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def rel(got, want):
    return abs(got - want) / max(abs(want), 5e-324)


@pytest.mark.parametrize(
    "alpha,delta,want",
    [
        (0.01, 0.05, 299),  # 0.99**298 = 0.0501 > 0.05 >= 0.99**299 = 0.0496
        (0.05, 0.05, 59),
        (0.1, 0.1, 22),
        (0.02, 0.05, 149),
        (0.001, 0.05, 2995),
        (0.05, 0.01, 90),
    ],
)
def test_min_calibration_n(alpha, delta, want):
    assert ltt_check.min_calibration_n(alpha, delta) == want
    # the returned n satisfies the bound and n-1 does not
    assert ltt_check.hb_pvalue(want, 0.0, alpha) <= delta
    assert ltt_check.hb_pvalue(want - 1, 0.0, alpha) > delta


@pytest.mark.parametrize("alpha,delta", [(0, 0.05), (1, 0.05), (0.01, 0), (0.01, 1), (-1, 0.5), (float("nan"), 0.05)])
def test_min_calibration_n_invalid_is_none(alpha, delta):
    assert ltt_check.min_calibration_n(alpha, delta) is None


@pytest.mark.parametrize(
    "n,rhat,alpha",
    [(1000, 0.05, 0.01), (1000, 0.01, 0.01), (10, 1.0, 0.5), (1, 1.0, 0.999), (200000, 0.02, 0.01)],
)
def test_pvalue_is_one_when_rhat_reaches_alpha(n, rhat, alpha):
    assert ltt_check.hb_pvalue(n, rhat, alpha) == 1.0


def test_pvalue_invalid_input_is_nan():
    nan = float("nan")
    for args in [(0, 0.1, 0.5), (-3, 0.1, 0.5), (10, -0.1, 0.5), (10, 1.1, 0.5), (10, nan, 0.5), (10, 0.1, 0.0), (10, 0.1, 1.0), (10, 0.1, nan)]:
        assert math.isnan(ltt_check.hb_pvalue(*args)), args


def test_pvalue_exponential_branch_at_zero_loss():
    # 0 ln 0 = 0: KL(0||alpha) = -ln(1-alpha), so the exp branch is (1-alpha)**n
    assert rel(ltt_check.hb_pvalue(10, 0.0, 0.5), 1.0 / 1024) < 1e-15
    assert rel(ltt_check.hb_pvalue(299, 0.0, 0.01), 0.99**299) < 1e-12
    assert ltt_check.hb_pvalue(1_000_000, 0.0, 0.01) == 0.0  # underflow is a reject, never NaN


def test_pvalue_binomial_branch():
    # n=20, alpha=.5, rhat=.25 -> k=5: e * 5425/262144 = 0.05625411575084808 < exp(-20 KL) = 0.0730770643366773
    assert rel(ltt_check.hb_pvalue(20, 0.25, 0.5), 0.0562541157508480850) < 1e-12


@pytest.mark.parametrize(
    "n,rhat,alpha,want",
    [
        (1000, 0.005, 0.01, 0.17978583254514914),
        (50, 0.04, 0.2, 0.0034941201094308096),
        (200000, 0.0099, 0.01, 0.9016176208084629),
        (1000, 0.003, 0.01, 0.027380314431107977),
    ],
)
def test_pvalue_against_exact_reference(n, rhat, alpha, want):
    assert rel(ltt_check.hb_pvalue(n, rhat, alpha), want) < 1e-9


def test_count_derived_rhat_uses_the_exact_count():
    # 200000 * (1980/200000) is 1980.0000000000002 in float64; the tail must stop at 1980.
    got = ltt_check.hb_pvalue(200000, 1980 / 200000, 0.01)
    assert rel(got, 0.9016176208084629) < 1e-9


def test_ltt_fixed_sequence_walk():
    grid = [0.01, 0.02, 0.03, 0.04, 0.05]
    risks = [0.0, 0.001, 0.003, 0.004, 0.0005]
    res = ltt_check.ltt(1000, risks, grid, 0.01, 0.05)
    assert res["refused"] is False
    assert res["lambda"] == 0.03
    assert res["need_n"] == 299 and res["have_n"] == 1000
    assert [p["rejected"] for p in res["points"]] == [True, True, True, False]
    want = [4.3171247410658251e-05, 1.1847771378538459e-03, 2.7380314431107977e-02, 7.7977719841202126e-02]
    for point, w in zip(res["points"], want):
        assert rel(point["p"], w) < 1e-9


def test_ltt_refuses_below_minimum_n_even_with_zero_risk():
    grid = [0.005 * i for i in range(1, 11)]
    res = ltt_check.ltt(298, [0.0] * len(grid), grid, 0.01, 0.05)
    assert res["refused"] is True
    assert res["need_n"] == 299 and res["have_n"] == 298
    ok = ltt_check.ltt(299, [0.0] * len(grid), grid, 0.01, 0.05)
    assert ok["refused"] is False and ok["lambda"] == grid[-1]


def test_ltt_refuses_when_first_lambda_fails():
    res = ltt_check.ltt(5000, [0.02, 0.0], [0.01, 0.02], 0.01, 0.05)
    assert res["refused"] is True
    assert len(res["points"]) == 1 and res["points"][0]["rejected"] is False


def test_ltt_rejects_a_grid_that_is_not_ascending():
    res = ltt_check.ltt(1000, [0.0, 0.0], [0.02, 0.01], 0.01, 0.05)
    assert res["refused"] is True and res["points"] == []


@pytest.mark.parametrize(
    "k,n,conf,want",
    [
        (0, 10, 0.95, 1 - 0.05 ** (1 / 10)),
        (1, 10, 0.95, 0.39416330243650478),
        (3, 50, 0.9, 0.12875642258660188),
        (5, 100, 0.99, 0.12585173069767864),
    ],
)
def test_clopper_pearson_upper(k, n, conf, want):
    assert abs(ltt_check.clopper_pearson_upper(k, n, conf) - want) < 1e-9


def test_clopper_pearson_edges():
    assert ltt_check.clopper_pearson_upper(10, 10, 0.95) == 1.0
    assert ltt_check.clopper_pearson_upper(11, 10, 0.95) == 1.0  # cannot evaluate: trip the alarm


def run_cli(tmp_path, doc):
    vectors = tmp_path / "in.json"
    vectors.write_text(json.dumps(doc), encoding="utf-8")
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1", PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [sys.executable, "-m", "hazard_research.ltt_check", "--vectors", str(vectors)],
        cwd=RESEARCH_DIR,
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
        check=False,
    )
    return proc


def test_cli_prints_json_results(tmp_path):
    doc = {
        "hb": [{"n": 1000, "rhat": 0.05, "alpha": 0.01}, {"n": 20, "rhat": 0.25, "alpha": 0.5}],
        "min_n": [{"alpha": 0.01, "delta": 0.05}],
        "ltt": [{"n": 1000, "alpha": 0.01, "delta": 0.05, "grid": [0.01, 0.02, 0.03, 0.04, 0.05], "risk": [0.0, 0.001, 0.003, 0.004, 0.0005]}],
        "cp": [{"k": 1, "n": 10, "confidence": 0.95}],
    }
    proc = run_cli(tmp_path, doc)
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert out["hb"][0]["p"] == 1.0
    assert rel(out["hb"][1]["p"], 0.0562541157508480850) < 1e-12
    assert out["min_n"][0]["n"] == 299
    assert out["ltt"][0]["lambda"] == 0.03 and out["ltt"][0]["refused"] is False
    assert len(out["ltt"][0]["points"]) == 4
    assert abs(out["cp"][0]["upper"] - 0.39416330243650478) < 1e-9


def test_cli_rejects_a_missing_vectors_file(tmp_path):
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    proc = subprocess.run(
        [sys.executable, "-m", "hazard_research.ltt_check", "--vectors", str(tmp_path / "nope.json")],
        cwd=RESEARCH_DIR,
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    assert proc.returncode != 0
