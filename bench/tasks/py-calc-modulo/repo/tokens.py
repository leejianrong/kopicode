"""Token kinds for the little expression language.

Adding an operator touches three files: the kind lives here, the character the
lexer recognises lives in lexer.py, and how it evaluates lives in evaluator.py.
"""

from dataclasses import dataclass

NUMBER = "NUMBER"
PLUS = "PLUS"
MINUS = "MINUS"
STAR = "STAR"
SLASH = "SLASH"
LPAREN = "LPAREN"
RPAREN = "RPAREN"


@dataclass(frozen=True)
class Token:
    """One lexed token: its kind and the source text it came from."""

    kind: str
    text: str
