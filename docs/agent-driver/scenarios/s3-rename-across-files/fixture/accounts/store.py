"""An in-memory user store."""

_USERS = {1: "ada", 2: "grace"}


def fetch_user(user_id: int) -> str:
    """Return the name for user_id. fetch_user raises KeyError for an unknown id."""
    return _USERS[user_id]
