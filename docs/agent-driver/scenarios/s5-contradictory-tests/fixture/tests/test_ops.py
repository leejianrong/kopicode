import unittest

from calc.ops import double


class OpsTest(unittest.TestCase):
    def test_double_two(self):
        self.assertEqual(double(2), 4)

    def test_double_two_again(self):
        # A copy-paste slip: this contradicts the test above.
        self.assertEqual(double(2), 5)
