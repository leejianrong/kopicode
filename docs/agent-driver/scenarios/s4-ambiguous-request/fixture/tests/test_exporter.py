import unittest

from export.exporter import to_csv


class ExporterTest(unittest.TestCase):
    def test_csv(self):
        self.assertEqual(to_csv([{"a": 1}]), "a\r\n1\r\n")

    def test_empty(self):
        self.assertEqual(to_csv([]), "")
