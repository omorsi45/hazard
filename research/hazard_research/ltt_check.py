"""Independent reference implementation of the Hoeffding-Bentkus LTT statistics.

Written from the formulas in the design (seed 10.2), not from the Go code, and
using only the standard library (math, json, sys, argparse). It exists so the
Go implementation can be cross-checked against a second, differently built
one:

    python -m hazard_research.ltt_check --vectors in.json

reads a JSON object with any of the lists

    "hb":    [{"n": int, "rhat": float, "alpha": float}]
    "min_n": [{"alpha": float, "delta": float}]
    "ltt":   [{"n": int, "alpha": float, "delta": float,
               "grid": [float], "risk": [float]}]    risk[i] is the empirical
                                                     miss rate at grid[i]
    "cp":    [{"k": int, "n": int, "confidence": float}]

and prints a JSON object with the same keys holding the results, in order.
"""

import argparse
import json
import math
import sys

# n*rhat within this relative distance of an integer is that integer (rhat is
# an empirical rate k/n, so the product is a whole count up to float rounding).
_COUNT_SLACK = 1e-12

# A binomial term this far (in log) below the largest one is below float
# resolution of the sum.
_NEGLIGIBLE = -50.0


def _tail_count(n, rhat):
    """ceil(n * rhat), reading a product within float rounding of a whole number as that number."""
    x = n * rhat
    nearest = math.floor(x + 0.5)
    if abs(x - nearest) <= _COUNT_SLACK * max(1.0, nearest):
        return min(int(nearest), n)
    return min(int(math.ceil(x)), n)


def _log_binom_cdf(k, n, p):
    """ln P(Bin(n, p) <= k) for 0 < p < 1.

    The terms are built upward from ln pmf(0) = n ln(1-p) with the ratio
    pmf(i)/pmf(i-1) = (n-i+1)/i * p/(1-p), kept in log space, and summed
    relative to the largest one.
    """
    if k < 0:
        return -math.inf
    if k >= n:
        return 0.0
    ln_p = math.log(p)
    ln_q = math.log1p(-p)
    term = n * ln_q
    terms = [term]
    for i in range(1, k + 1):
        term += math.log((n - i + 1) / i) + ln_p - ln_q
        terms.append(term)
    top = max(terms)
    return top + math.log(math.fsum(math.exp(t - top) for t in terms if t - top > _NEGLIGIBLE))


def hb_pvalue(n, rhat, alpha):
    """Hoeffding-Bentkus p-value for H : R > alpha; NaN for invalid input."""
    if not (n > 0 and 0.0 <= rhat <= 1.0 and 0.0 < alpha < 1.0):
        return math.nan
    if rhat >= alpha:
        return 1.0
    kl = (1.0 - rhat) * math.log((1.0 - rhat) / (1.0 - alpha))
    if rhat > 0.0:
        kl += rhat * math.log(rhat / alpha)
    chernoff = math.exp(-n * kl)
    binomial = math.e * math.exp(_log_binom_cdf(_tail_count(n, rhat), n, alpha))
    return min(1.0, max(0.0, min(chernoff, binomial)))


def min_calibration_n(alpha, delta):
    """Smallest n with hb_pvalue(n, 0, alpha) <= delta, or None if invalid or unreachable."""
    if not (0.0 < alpha < 1.0 and 0.0 < delta < 1.0):
        return None
    # (1-alpha)**n <= delta  <=>  n >= ln(delta) / ln(1-alpha); then settle the
    # boundary against the p-value itself.
    n = max(1, int(math.ceil(math.log(delta) / math.log1p(-alpha))))
    if n > 1 << 40:
        return None
    while n > 1 and hb_pvalue(n - 1, 0.0, alpha) <= delta:
        n -= 1
    while not hb_pvalue(n, 0.0, alpha) <= delta:
        n += 1
        if n > 1 << 40:
            return None
    return n


def _ascending(grid):
    if not grid:
        return False
    for i, lam in enumerate(grid):
        if not math.isfinite(lam):
            return False
        if i and not lam > grid[i - 1]:
            return False
    return True


def ltt(n, risks, grid, alpha, delta):
    """Fixed-sequence Learn-then-Test over an ascending grid.

    risks[i] is the empirical miss rate at grid[i]. Walks from the smallest
    lambda, rejecting while the p-value is <= delta and stopping at the first
    non-rejection. Returns a dict with lambda (0.0 when refused), refused,
    need_n, have_n and points ({lambda, risk, p, rejected}, ending at the first
    non-rejection). Invalid arguments refuse without evaluating any lambda.
    """
    need = min_calibration_n(alpha, delta)
    result = {"lambda": 0.0, "refused": True, "need_n": need, "have_n": n, "points": [], "empirical_miss": 1.0}
    if n <= 0 or need is None or not _ascending(grid) or len(risks) != len(grid):
        return result
    points = []
    for lam, r in zip(grid, risks):
        p = hb_pvalue(n, r, alpha)
        rejected = p <= delta  # False for NaN
        points.append({"lambda": lam, "risk": r, "p": p, "rejected": rejected})
        if not rejected:
            break
    result["points"] = points
    certified = [pt for pt in points if pt["rejected"]]
    if certified:
        last = certified[-1]
        result.update({"lambda": last["lambda"], "refused": False, "empirical_miss": last["risk"]})
    return result


def clopper_pearson_upper(k, n, confidence):
    """One-sided upper Clopper-Pearson bound: the p with P(Bin(n, p) <= k) = 1 - confidence.

    Returns 1.0 when k == n and for input that cannot be evaluated.
    """
    if not (n > 0 and 0 <= k <= n and 0.0 < confidence < 1.0) or k == n:
        return 1.0
    target = math.log1p(-confidence)
    if k == 0:
        return -math.expm1(target / n)
    lo, hi = 0.0, 1.0
    for _ in range(200):
        mid = (lo + hi) / 2.0
        if not lo < mid < hi:
            break
        if _log_binom_cdf(k, n, mid) > target:
            lo = mid
        else:
            hi = mid
    return hi


def _num(x):
    """JSON-safe float: NaN becomes null."""
    return None if isinstance(x, float) and math.isnan(x) else x


def run_vectors(doc):
    out = {}
    if "hb" in doc:
        out["hb"] = [{"p": _num(hb_pvalue(v["n"], v["rhat"], v["alpha"]))} for v in doc["hb"]]
    if "min_n" in doc:
        out["min_n"] = [{"n": min_calibration_n(v["alpha"], v["delta"])} for v in doc["min_n"]]
    if "ltt" in doc:
        results = []
        for v in doc["ltt"]:
            res = ltt(v["n"], v["risk"], v["grid"], v["alpha"], v["delta"])
            res["points"] = [dict(pt, p=_num(pt["p"])) for pt in res["points"]]
            results.append(res)
        out["ltt"] = results
    if "cp" in doc:
        out["cp"] = [{"upper": clopper_pearson_upper(v["k"], v["n"], v["confidence"])} for v in doc["cp"]]
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(prog="python -m hazard_research.ltt_check", description=__doc__.split("\n")[0])
    parser.add_argument("--vectors", required=True, help="JSON file of input vectors")
    args = parser.parse_args(argv)
    with open(args.vectors, "r", encoding="utf-8") as fh:
        doc = json.load(fh)
    json.dump(run_vectors(doc), sys.stdout, allow_nan=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
