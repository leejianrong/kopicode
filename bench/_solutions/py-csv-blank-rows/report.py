"""Sales report helpers used by the nightly job."""

import csv
import io


def parse_rows(text):
    """Parse CSV text into a list of (region, amount) pairs.

    The first line is a header and is dropped. Blank lines are skipped.
    """
    reader = csv.reader(io.StringIO(text))
    rows = list(reader)[1:]
    return [
        (row[0], float(row[1]))
        for row in rows
        if row and any(field.strip() for field in row)
    ]


def total_by_region(rows):
    """Sum the amounts per region, keeping first-seen order."""
    totals = {}
    for region, amount in rows:
        totals[region] = totals.get(region, 0.0) + amount
    return totals


def format_total(region, amount):
    """Render one report line: region left-aligned, amount right-aligned."""
    return f"{region:<10}{amount:>10.2f}"
