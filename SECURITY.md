# Security policy

## Reporting a vulnerability

Report vulnerabilities through GitHub private vulnerability reporting:
open the Security tab on https://github.com/levee-ai/levee and choose
"Report a vulnerability". Reports stay private between you and the
maintainer. Please do not open public issues for security problems.

Include what you can: the affected code path, a proof of concept, and the
impact you see. Budget-accounting bypasses (any way an agent can spend
without being counted) are in scope and treated as vulnerabilities, not bugs.

You will get an acknowledgment within 7 days.

## Supported versions

Levee is pre-release. Only the main branch is supported. After the first
tagged release this table will list supported versions.

## Scope notes

Levee passes provider API keys through untouched and never stores them. The
admin API is unauthenticated by design in the MVP and must stay on loopback,
see the security considerations section of the README for the full model.
