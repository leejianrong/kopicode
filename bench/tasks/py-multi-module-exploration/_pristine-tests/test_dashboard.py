import unittest

import cache
from report import dashboard, status_line
from resolver import resolve


class TestResolve(unittest.TestCase):
    def setUp(self):
        cache.clear()

    def test_defaults_only(self):
        self.assertEqual(
            resolve("dev"),
            {"workers": 2, "timeout": 30, "log_level": "INFO"},
        )

    def test_overrides_merge_over_defaults(self):
        self.assertEqual(
            resolve("staging"),
            {"workers": 4, "timeout": 30, "log_level": "DEBUG"},
        )

    def test_a_second_environment_is_not_stuck_on_the_first(self):
        self.assertEqual(resolve("staging")["workers"], 4)
        self.assertEqual(resolve("production")["workers"], 16)


class TestStatusLine(unittest.TestCase):
    def setUp(self):
        cache.clear()

    def test_status_line_reflects_its_own_environment(self):
        self.assertIn("workers=4", status_line("staging"))
        self.assertIn("workers=16", status_line("production"))


class TestDashboard(unittest.TestCase):
    def setUp(self):
        cache.clear()

    def test_dashboard_reflects_the_environments_asked_for(self):
        first = dashboard(("staging",))
        second = dashboard(("production",))
        self.assertIn("workers=4", first)
        self.assertIn("workers=16", second)
        self.assertNotEqual(first, second)

    def test_dashboard_of_one_matches_its_own_status_line(self):
        cache.clear()
        self.assertEqual(dashboard(("production",)), status_line("production"))


if __name__ == "__main__":
    unittest.main()
