# Security policy

OpenRDS manages databases and their credentials, so we take security reports seriously. Thank you for helping keep its users safe.

## Supported versions

Security fixes go into the `main` branch. Update by running the install command again, or `sh install.sh` in your copy; your databases, backups and accounts are kept.

## Reporting a vulnerability

Please **don't** open a public issue, discussion or pull request for a security problem.

- Use **Report a vulnerability** on the repository's [Security tab](https://github.com/kerjakuyco/openrds/security) to send a private report.
- If that option isn't available, open an issue titled "Security contact" **without any details**, and we will reply with a private way to reach us.

Please include what you found, the steps to reproduce it, the version (commit) you tested, and the impact you expect. We'll confirm the report, keep you updated while we fix it, and credit you in the release notes if you'd like.

## Scope and trust model

- Anyone with shell access to the server, access to the Docker socket, the API key, or an administrator account is trusted to operate every database. Reports that require those are usually out of scope.
- The console and databases listen on the server's loopback interface by default. A database becomes reachable from outside only when an operator turns on public access, and then only from the allowed addresses.
- Reports about the console, the HTTP API, credential handling, TLS, the public-access gateway and backup storage are all in scope.
