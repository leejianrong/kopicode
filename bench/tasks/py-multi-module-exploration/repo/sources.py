"""Static configuration data: defaults and the per-environment overrides.

This module is intentionally dumb — no caching, no computation, nothing that
depends on when or how many times it is called. Add a new environment's
overrides here.
"""

DEFAULTS = {
    "workers": 2,
    "timeout": 30,
    "log_level": "INFO",
}

OVERRIDES = {
    "staging": {"workers": 4, "log_level": "DEBUG"},
    "production": {"workers": 16, "timeout": 60},
}


def overrides_for(environment):
    """Return the override dict for an environment, or {} if it has none."""
    return OVERRIDES.get(environment, {})
