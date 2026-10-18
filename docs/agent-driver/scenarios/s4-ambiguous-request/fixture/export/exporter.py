"""Exports rows (lists of dicts) to text."""
import csv
import io


def to_csv(rows: list) -> str:
    if not rows:
        return ""
    out = io.StringIO()
    writer = csv.DictWriter(out, fieldnames=list(rows[0]))
    writer.writeheader()
    writer.writerows(rows)
    return out.getvalue()
