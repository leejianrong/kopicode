import unittest


class DataTest(unittest.TestCase):
    def test_generated_file_has_the_answer(self):
        with open("build/data.txt") as f:
            self.assertEqual(f.read().strip(), "42")


if __name__ == "__main__":
    unittest.main()
