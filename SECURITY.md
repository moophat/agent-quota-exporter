# Security policy

agent-quota-exporter reads agent state and, for opt-in sources, credential files on the machine it runs on. A bug
that leaks a token, writes to agent state, follows a link into another user's files or exposes message content is a
security issue.

## Reporting a vulnerability

Please report privately, not in a public issue:

- use GitHub's **"Report a vulnerability"** button on the repository's Security tab (private vulnerability
  reporting), or
- if that is unavailable, open an issue that says only that you have a security report and asks for a private
  channel, without any details.

Include the version (`agent-quota-exporter --version`), the affected source or config, and the steps to reproduce.
Never include a real token, credential file or transcript; synthetic values are enough.

We aim to acknowledge a report within 7 days and to agree on a disclosure date with you once a fix is available.

## Supported versions

Only the latest release (including release candidates before 1.0) receives security fixes.

## Scope

In scope: the exporter binary, its config handling and the shipped examples. Out of scope: the agent harnesses and
vendor endpoints it reads, and deployments that expose `/metrics` to untrusted networks against the guidance in the
README.
