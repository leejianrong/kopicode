from accounts.store import fetch_user


def greet(user_id: int) -> str:
    # fetch_user is the only lookup we have.
    return f"hello, {fetch_user(user_id)}"
