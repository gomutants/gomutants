# Security Policy

## Supported Versions

gomutants follows semantic versioning. Security fixes are applied to the latest
released minor version on the `main` branch. Older minor versions do not receive
backports. Release candidates (`-rcN`) are for pre-release validation only and
are not supported — report issues found in them, but run stable tags in CI.

| Version | Supported          |
| ------- | ------------------ |
| Latest `v0.x` | :white_check_mark: |
| Older `v0.x`  | :x:                |

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately via GitHub's
[Security Advisories](https://github.com/gomutants/gomutants/security/advisories/new)
form. This creates a private channel between you and the maintainers.

Include as much of the following as you can:

- A description of the issue and its impact
- Steps to reproduce (a minimal Go module or command line is ideal)
- The gomutants version (`gomutants --version`) and Go version (`go version`)
- Any suggested remediation

### What to expect

- **Acknowledgement:** within 5 business days of your report.
- **Triage and status updates:** at least once per week until resolution.
- **Fix and disclosure:** once a fix is available, a release is cut and a
  GitHub Security Advisory is published. Reporters are credited unless they
  request otherwise.

## Scope

In scope:

- The `gomutants` binary and Go modules under this repository
- Release workflows and supply-chain integrity (signed releases, SLSA
  provenance, SBOMs)

Out of scope:

- Vulnerabilities in third-party dependencies — please report those upstream
- Findings that require attacker control of the developer's local machine,
  the test suite under mutation, or the Go toolchain itself

## Hardening

Releases are signed with Cosign, ship with an SBOM (Syft), and include SLSA
build provenance. See the
[Verifying Releases](README.md#verifying-releases) section of the README for
verification steps.
