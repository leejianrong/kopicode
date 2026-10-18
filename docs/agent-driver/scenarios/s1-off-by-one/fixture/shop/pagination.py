"""Pagination helpers for the product listing."""


def page_count(total_items: int, per_page: int) -> int:
    """How many pages are needed to show total_items, per_page at a time."""
    if per_page <= 0:
        raise ValueError("per_page must be positive")
    return total_items // per_page


def page_slice(items: list, page: int, per_page: int) -> list:
    """The items on a 1-based page."""
    start = (page - 1) * per_page
    return items[start : start + per_page]
