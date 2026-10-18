from accounts import store


def names(ids: list) -> list:
    return [store.fetch_user(i) for i in ids]
