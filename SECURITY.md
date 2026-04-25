# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in this project, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please send an email to the project maintainers with:

1. A description of the vulnerability
2. Steps to reproduce the issue
3. Any potential impact assessment

We will acknowledge receipt within 48 hours and aim to provide a fix within 7 days for critical issues.

## Supported Versions

Only the latest release is supported with security updates.

## Security Practices

- API authentication via bearer token or X-Api-Key header
- Constant-time key comparison to prevent timing attacks
- Rate limiting per IP address
- Query depth limiting to prevent GraphQL abuse
- Request body size limits
- SQL injection prevention via parameterized queries
- Regex pattern length limits to prevent ReDoS
