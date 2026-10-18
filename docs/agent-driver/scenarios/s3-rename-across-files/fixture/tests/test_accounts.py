import unittest

from accounts.greeting import greet
from accounts.report import names
from accounts import store


class AccountsTest(unittest.TestCase):
    def test_lookup_goes_through_fetch_user(self):
        self.assertEqual(store.fetch_user(1), "ada")

    def test_unknown_user(self):
        with self.assertRaises(KeyError):
            store.fetch_user(99)

    def test_greet(self):
        self.assertEqual(greet(2), "hello, grace")

    def test_names(self):
        self.assertEqual(names([1, 2]), ["ada", "grace"])
