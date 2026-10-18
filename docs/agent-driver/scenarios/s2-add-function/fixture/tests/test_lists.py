import unittest

from textutil.lists import first, last


class ListsTest(unittest.TestCase):
    def test_first(self):
        self.assertEqual(first([4, 5]), 4)
        self.assertIsNone(first([]))

    def test_last(self):
        self.assertEqual(last([4, 5]), 5)
        self.assertEqual(last([], default=0), 0)
