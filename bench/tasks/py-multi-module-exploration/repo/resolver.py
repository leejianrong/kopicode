"""Resolves the effective settings for one environment.

Merging the defaults with an environment's overrides is cheap, but
`resolve` is called once per status line rendered and the dashboard renders
often, so the merged result is memoized rather than recomputed every time.
"""

from cache import get_or_compute
from sources import DEFAULTS, overrides_for


def resolve(environment):
    """Return the fully merged settings for environment."""

    def compute():
        merged = dict(DEFAULTS)
        merged.update(overrides_for(environment))
        return merged

    return get_or_compute("resolved-settings", compute)
