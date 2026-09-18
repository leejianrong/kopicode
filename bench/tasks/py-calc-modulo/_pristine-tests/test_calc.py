import unittest

from evaluator import ParseError, evaluate
from lexer import LexError, tokenize
from tokens import NUMBER, PERCENT


def value(source):
    return evaluate(tokenize(source))


class TestLexer(unittest.TestCase):
    def test_modulo_is_a_token(self):
        kinds = [token.kind for token in tokenize("7 % 3")]
        self.assertEqual(kinds, [NUMBER, PERCENT, NUMBER])

    def test_unknown_character(self):
        with self.assertRaises(LexError):
            tokenize("2 $ 3")


class TestEvaluator(unittest.TestCase):
    def test_existing_operators(self):
        self.assertEqual(value("2 + 3 * 4"), 14)
        self.assertEqual(value("(2 + 3) * 4"), 20)
        self.assertEqual(value("9 / 2"), 4)
        self.assertEqual(value("10 - 4 - 3"), 3)

    def test_modulo(self):
        self.assertEqual(value("7 % 3"), 1)
        self.assertEqual(value("10 % 5"), 0)

    def test_modulo_binds_tighter_than_plus(self):
        self.assertEqual(value("2 + 7 % 3"), 3)

    def test_modulo_associates_left(self):
        self.assertEqual(value("13 % 7 % 4"), 2)

    def test_trailing_operator(self):
        with self.assertRaises(ParseError):
            value("7 %")


if __name__ == "__main__":
    unittest.main()
