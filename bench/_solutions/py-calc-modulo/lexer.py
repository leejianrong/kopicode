"""Turns source text into a list of tokens."""

from tokens import LPAREN, MINUS, NUMBER, PERCENT, PLUS, RPAREN, SLASH, STAR, Token

# Single-character tokens, keyed by the character the lexer reads.
SYMBOLS = {
    "+": PLUS,
    "-": MINUS,
    "*": STAR,
    "/": SLASH,
    "%": PERCENT,
    "(": LPAREN,
    ")": RPAREN,
}


class LexError(Exception):
    """Raised when the source contains a character the lexer does not know."""


def tokenize(text):
    """Return the tokens in text. Whitespace separates but is not a token."""
    tokens = []
    i = 0
    while i < len(text):
        ch = text[i]

        if ch.isspace():
            i += 1
            continue

        if ch.isdigit():
            start = i
            while i < len(text) and text[i].isdigit():
                i += 1
            tokens.append(Token(NUMBER, text[start:i]))
            continue

        kind = SYMBOLS.get(ch)
        if kind is None:
            raise LexError(f"unexpected character {ch!r} at position {i}")
        tokens.append(Token(kind, ch))
        i += 1

    return tokens
