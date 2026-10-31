"""Independent recomputation of the reliability table of hazard eval.

Written from the definition in the design (seed 12.1: 10 equal-width bins of
the predicted probability, with the observed failure rate of each), not from
the Go code, and using only the standard library. It reads the pairs.csv that
hazard eval writes and prints the bins of every configuration:

    python -m hazard_research.reliability --pairs pairs.csv

The output is {"configs": {"P0": [{"bin", "lo", "hi", "count", "mean_p",
"observed_rate"}, ...]}}. An empty bin has no mean and no rate: they are null,
never 0. The first lines of the file may be comments starting with "#".
"""

import argparse
import csv
import json
import math
import sys

BINS = 10
COLUMNS = ("event", "test", "p", "label", "config")


def bin_of(p, bins=BINS):
    """Bin of a probability: floor(p * bins), with 1.0 in the last bin."""
    if not (isinstance(p, float) and 0.0 <= p <= 1.0):
        raise ValueError("%r is not a probability in [0, 1]" % (p,))
    return min(int(math.floor(p * bins)), bins - 1)


def read_pairs(path):
    """Rows of a pairs.csv as dicts with event, test, p, label and config."""
    with open(path, "r", encoding="utf-8", newline="") as fh:
        lines = [line for line in fh if not line.startswith("#")]
    reader = csv.reader(lines)
    try:
        header = next(reader)
    except StopIteration:
        return []
    missing = [name for name in COLUMNS if name not in header]
    if missing:
        raise ValueError("%s lacks the columns %s" % (path, ", ".join(missing)))
    index = {name: header.index(name) for name in COLUMNS}
    rows = []
    for number, record in enumerate(reader, start=2):
        if len(record) != len(header):
            raise ValueError("%s: record %d has %d fields, want %d" % (path, number, len(record), len(header)))
        try:
            p = float(record[index["p"]])
        except ValueError:
            raise ValueError("%s: record %d: %r is not a number" % (path, number, record[index["p"]])) from None
        if not (0.0 <= p <= 1.0):
            raise ValueError("%s: record %d: %r is not a probability in [0, 1]" % (path, number, p))
        label = record[index["label"]]
        if label not in ("0", "1"):
            raise ValueError("%s: record %d: the label is %r, want 0 or 1" % (path, number, label))
        rows.append(
            {
                "event": record[index["event"]],
                "test": record[index["test"]],
                "p": p,
                "label": int(label),
                "config": record[index["config"]],
            }
        )
    return rows


def reliability(rows, bins=BINS):
    """The bins of each configuration, keyed by configuration in order of first appearance."""
    count = {}
    total_p = {}
    total_y = {}
    for row in rows:
        config = row["config"]
        if config not in count:
            count[config] = [0] * bins
            total_p[config] = [0.0] * bins
            total_y[config] = [0] * bins
        b = bin_of(row["p"], bins)
        count[config][b] += 1
        total_p[config][b] += row["p"]
        total_y[config][b] += row["label"]
    out = {}
    for config in count:
        table = []
        for i in range(bins):
            n = count[config][i]
            table.append(
                {
                    "bin": i,
                    "lo": i / bins,
                    "hi": (i + 1) / bins,
                    "count": n,
                    "mean_p": total_p[config][i] / n if n else None,
                    "observed_rate": total_y[config][i] / n if n else None,
                }
            )
        out[config] = table
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(prog="python -m hazard_research.reliability", description=__doc__.split("\n")[0])
    parser.add_argument("--pairs", required=True, help="the pairs.csv written by hazard eval")
    parser.add_argument("--bins", type=int, default=BINS, help="number of equal-width bins")
    args = parser.parse_args(argv)
    json.dump({"configs": reliability(read_pairs(args.pairs), args.bins)}, sys.stdout, allow_nan=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
