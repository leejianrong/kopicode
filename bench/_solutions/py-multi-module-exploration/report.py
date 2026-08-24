"""Renders the status dashboard shown to the on-call team."""

from cache import get_or_compute
from resolver import resolve


def status_line(environment):
    """Return a one-line summary of environment's effective settings."""
    settings = resolve(environment)
    return (
        f"{environment}: workers={settings['workers']}, "
        f"timeout={settings['timeout']}, log_level={settings['log_level']}"
    )


def dashboard(environments):
    """Return the full dashboard: one status line per environment, joined.

    Building the dashboard means rendering a status line for every
    environment, which is cheap on its own but happens on every refresh, so
    the joined text is memoized.
    """

    def compute():
        return "\n".join(status_line(environment) for environment in environments)

    return get_or_compute(("dashboard", environments), compute)
