import unittest

from shop.pagination import page_count, page_slice


class PaginationTest(unittest.TestCase):
    def test_exact_multiple(self):
        self.assertEqual(page_count(20, 10), 2)

    def test_partial_last_page_counts(self):
        self.assertEqual(page_count(21, 10), 3)

    def test_empty_listing_has_no_pages(self):
        self.assertEqual(page_count(0, 10), 0)

    def test_bad_page_size(self):
        with self.assertRaises(ValueError):
            page_count(5, 0)

    def test_slice(self):
        self.assertEqual(page_slice(list(range(25)), 3, 10), [20, 21, 22, 23, 24])
