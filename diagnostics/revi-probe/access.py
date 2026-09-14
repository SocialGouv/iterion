"""Select active report grants for the requesting tenant.

The caller has already authenticated tenant_id. This module does not perform
I/O; callers fetch returned grant IDs and expose their reports to that tenant.
See CONTRACT.md for the authorization and expiration rules.
"""
from dataclasses import dataclass


@dataclass(frozen=True)
class Grant:
    grant_id: str
    tenant_id: str
    expires_at: int


def is_current(grant: Grant, now: int) -> bool:
    """A report grant expires at its exact expiry timestamp."""
    return grant.expires_at >= now


def active_grants(grants: list[Grant], tenant_id: str, now: int) -> list[str]:
    """Return eligible grant IDs in the original order."""
    return [
        grant.grant_id
        for grant in grants
        if is_current(grant, now)
    ]
