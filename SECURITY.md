# Security policy

## Reporting a vulnerability

Please report security problems **privately**, not in a public issue. A public issue discloses the problem to everyone the moment you submit it.

Use GitHub's private vulnerability reporting: open this repository's **Security** tab and choose **Report a vulnerability**. Include what you found, how to reproduce it, and which version or snapshot you used.

I will acknowledge a report as soon as I can, but ws is a one-person project, so I cannot promise a response time. Please give me a reasonable chance to fix a problem before you disclose it.

## What to know first

ws has not had an independent security review. It is built for one owner and a few trusted people, not for untrusted tenants. [The trust page](docs/TRUST.md) lists what it protects and its known limits, so you can tell a known limit from a new finding.

## Supported versions

Only the latest snapshot of the main branch is supported.

## Please do not

- Test against a deployment you do not own.
- Include real secrets, tokens or personal data in a report.
