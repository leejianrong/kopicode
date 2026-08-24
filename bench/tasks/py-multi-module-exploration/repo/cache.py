"""A tiny memoization cache shared by whatever wants to avoid recomputing
something expensive.

The cache has no opinion about what makes a good key — that is entirely the
caller's job. It only promises that the same key returns the same stored
value without calling `compute` again. A caller that reuses one key for
calls that should produce different results will get the first result back
forever, which is a caller bug, not a cache bug.
"""

_store = {}


def get_or_compute(key, compute):
    """Return the cached value for key, computing and storing it on a miss."""
    if key not in _store:
        _store[key] = compute()
    return _store[key]


def clear():
    """Drop every cached value. Mostly useful for tests."""
    _store.clear()
