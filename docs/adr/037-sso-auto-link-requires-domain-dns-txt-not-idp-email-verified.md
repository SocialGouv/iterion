# ADR-037: SSO auto-link requires DNS-verified domain, not only IdP email_verified

- **Status**: Accepted
- **Date**: 2026-06-22
- **Authors**: Adry
- **Code**: [pkg/auth/oidc_service.go](../../pkg/auth/oidc_service.go), [pkg/auth/orgsso/domain.go](../../pkg/auth/orgsso/domain.go), [pkg/server/org_sso_domain_routes.go](../../pkg/server/org_sso_domain_routes.go)

## Context

Auto-linking a fresh SSO identity onto an existing iterion user by email is sensitive: if an org-controlled IdP can assert an email address it does not control, it could take over an unrelated existing account.

The IdP's `email_verified` claim is not sufficient in a tenant-controlled IdP model. The org controls the IdP configuration and signing keys, so the claim proves what the IdP says, not that the org owns the email domain.

## Decision

The SSO service's auto-link gate in [`pkg/auth/oidc_service.go`](../../pkg/auth/oidc_service.go) requires `AutoLinkOnEmail`, a domain store, a non-super-admin target, and a positive `IsVerifiedForTenant` result for the email domain. If these conditions are not met, existing-account linking requires explicit consent instead of automatic linking.

Domain ownership is represented by `VerifiedDomain` in [`pkg/auth/orgsso/domain.go`](../../pkg/auth/orgsso/domain.go). A tenant proves a domain through a DNS TXT challenge using a generated token and the `_iterion-challenge.` host convention.

The HTTP routes in [`pkg/server/org_sso_domain_routes.go`](../../pkg/server/org_sso_domain_routes.go) expose the domain verification lifecycle so org admins can create, inspect, verify, and manage the DNS-backed domain claims.

## Trade-offs

| Dimension | Require DNS-verified domain | Trust IdP `email_verified` only |
|---|---|---|
| Account takeover resistance | Requires proof the org controls the email domain. | A malicious tenant IdP can assert unowned verified emails. |
| Admin friction | Org must publish a DNS TXT record. | No DNS setup. |
| Tenant boundary | Links are constrained by domain ownership. | Tenant-controlled IdP claims cross tenant boundaries. |

The honest concession is that safe auto-linking requires extra DNS setup before the convenience feature works.

## Alternatives considered

### 1. Auto-link on IdP `email_verified`

The service could have linked any existing iterion account whose email matched a verified email claim from the org IdP.

**Rejected because**: the org controls its IdP claims and could assert `email_verified` for an email outside its domain authority.

### 2. Disable auto-link entirely

All existing-account SSO links could have required explicit user consent.

**Rejected because**: orgs that can prove domain ownership have a legitimate low-friction onboarding case, and DNS verification supplies the missing ownership proof.

## Consequences

- **Email auto-linking is tenant-safe by default.** A tenant IdP alone cannot claim unrelated email addresses for account takeover.
- **Domain verification becomes an SSO prerequisite.** Admins must complete DNS TXT verification before auto-link-on-email has effect.
- **`email_verified` still has limited value.** It may describe IdP-side email state, but it is not the ownership gate for cross-account linking.
- **Consent remains the fallback.** Existing accounts that do not pass the domain gate require explicit linking consent.
- **Rechallenge after stronger consent flows.** Hardened verification plus explicit user consent might make `email_verified` sufficient for some lower-risk linking paths.

## Status note (2026-10-14)

`canAutoLink` ([pkg/auth/oidc_service.go](../../pkg/auth/oidc_service.go)) adds
a fifth condition to the four above: the target must not be admin or owner of
any other non-personal team (a personal team is skipped), so a link cannot
escalate beyond the linking org. The gate also fails closed when the
membership read errors.
