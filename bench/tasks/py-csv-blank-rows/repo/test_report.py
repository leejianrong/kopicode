import unittest

from report import format_total, parse_rows, total_by_region

CLEAN = "region,amount\nnorth,10\nsouth,5\n"
WITH_BLANKS = "region,amount\nnorth,10\n\nsouth,5\n   \n"


class TestParseRows(unittest.TestCase):
    def test_clean_input(self):
        self.assertEqual(parse_rows(CLEAN), [("north", 10.0), ("south", 5.0)])

    def test_blank_lines_are_skipped(self):
        self.assertEqual(parse_rows(WITH_BLANKS), [("north", 10.0), ("south", 5.0)])

    def test_header_only(self):
        self.assertEqual(parse_rows("region,amount\n"), [])


class TestTotals(unittest.TestCase):
    def test_totals_are_summed_per_region(self):
        rows = [("north", 10.0), ("south", 5.0), ("north", 2.5)]
        self.assertEqual(total_by_region(rows), {"north": 12.5, "south": 5.0})

    def test_format_total(self):
        self.assertEqual(format_total("north", 12.5), "north          12.50")


if __name__ == "__main__":
    unittest.main()
