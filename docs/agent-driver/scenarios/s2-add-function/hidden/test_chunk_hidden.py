import unittest

from textutil.lists import chunk


class ChunkHidden(unittest.TestCase):
    def test_basic(self):
        self.assertEqual(chunk([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]])

    def test_exact(self):
        self.assertEqual(chunk([1, 2, 3, 4], 2), [[1, 2], [3, 4]])

    def test_bigger_than_list(self):
        self.assertEqual(chunk([1, 2], 5), [[1, 2]])

    def test_empty(self):
        self.assertEqual(chunk([], 3), [])

    def test_bad_size(self):
        for bad in (0, -1):
            with self.assertRaises(ValueError):
                chunk([1], bad)

    def test_input_unchanged(self):
        items = [1, 2, 3]
        chunk(items, 2)
        self.assertEqual(items, [1, 2, 3])
