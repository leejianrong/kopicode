"""Recursive-descent evaluator over the token stream.

Integer arithmetic only: division truncates towards negative infinity, which is
what Python's // already does.
"""

from tokens import LPAREN, MINUS, NUMBER, PERCENT, PLUS, RPAREN, SLASH, STAR

# Operators that bind tighter than + and -.
PRODUCT_OPS = (STAR, SLASH, PERCENT)


class ParseError(Exception):
    """Raised when the token stream is not a well-formed expression."""


def evaluate(tokens):
    """Evaluate a token list and return its integer value."""
    value, rest = _sum(tokens)
    if rest:
        raise ParseError(f"unexpected trailing token {rest[0].text!r}")
    return value


def _sum(tokens):
    value, tokens = _product(tokens)
    while tokens and tokens[0].kind in (PLUS, MINUS):
        op, tokens = tokens[0], tokens[1:]
        right, tokens = _product(tokens)
        value = value + right if op.kind == PLUS else value - right
    return value, tokens


def _product(tokens):
    value, tokens = _atom(tokens)
    while tokens and tokens[0].kind in PRODUCT_OPS:
        op, tokens = tokens[0], tokens[1:]
        right, tokens = _atom(tokens)
        value = _apply(op.kind, value, right)
    return value, tokens


def _apply(kind, left, right):
    if kind == STAR:
        return left * right
    if kind == SLASH:
        return left // right
    if kind == PERCENT:
        return left % right
    raise ParseError(f"not a product operator: {kind}")


def _atom(tokens):
    if not tokens:
        raise ParseError("unexpected end of input")

    head, tokens = tokens[0], tokens[1:]

    if head.kind == NUMBER:
        return int(head.text), tokens

    if head.kind == LPAREN:
        value, tokens = _sum(tokens)
        if not tokens or tokens[0].kind != RPAREN:
            raise ParseError("missing closing parenthesis")
        return value, tokens[1:]

    raise ParseError(f"unexpected token {head.text!r}")
