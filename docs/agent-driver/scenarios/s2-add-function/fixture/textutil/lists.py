"""Small list helpers."""


def first(items: list, default=None):
    """The first item, or default for an empty list."""
    return items[0] if items else default


def last(items: list, default=None):
    """The last item, or default for an empty list."""
    return items[-1] if items else default
