# Security Policy

## Reporting a vulnerability

Report privately to **security@example.invalid**. Please do not open a public
issue, discussion, or pull request for a suspected vulnerability.

Include what you need to make the report actionable: affected version or commit,
component (server, SDK, MCP server, connector), reproduction steps or a proof of
concept, and your assessment of the impact.

If you would like a reply encrypted, say so in your first message and we will
exchange keys.

## What to expect

| Stage | Commitment |
|---|---|
| Acknowledgement of your report | Within 3 business days |
| Initial assessment and severity | Within 10 business days |
| Progress updates | At least every 14 days until resolved |
| Fix for critical and high severity | Target 30 days from confirmation |
| Fix for medium and low severity | Next scheduled release |

We will tell you when a fix ships and credit you in the advisory unless you ask
us not to. We ask that you give us a reasonable opportunity to release a fix
before disclosing publicly; we will not pursue or support legal action against
researchers acting in good faith under this policy.

## Supported versions

| Version | Supported |
|---|---|
| `main` | Yes |
| Tagged releases | None yet |

Phase 1 is in development and the project has not cut a release. Until it does,
security fixes land on `main` only. This table will list supported release lines
once versioning begins.

## Scope

**In scope:** the server and its HTTP API, the MCP server, the web UI,
authentication and the enrollment and agent credential model, multi-tenancy
isolation at the data layer, the audit log, the Python and TypeScript SDKs, and
the discovery connectors when they land.

**Out of scope:** findings against a deployment you do not operate, denial of
service from raw traffic volume, missing hardening headers with no demonstrated
impact, vulnerabilities in dependencies with no exploitable path through this
code, and social engineering.

## What AI Inventory does not hold

This materially changes the threat model of any finding, so it is worth stating
plainly:

**AI Inventory never collects prompts, completions, conversation content,
end-user data, or model inputs and outputs.** There is no code path in any SDK
that can transmit user data, and this is a hard product boundary rather than a
configuration setting.

What the system does hold is metadata about agents — names, descriptions,
owning teams, environments, frameworks, hosting providers and regions,
repository and commit identifiers, telemetry links, and the audit history of
changes to all of it. It also holds credential material: enrollment tokens and
per-agent credentials, stored only as hashes and displayed exactly once at
creation.

A compromise of AI Inventory therefore exposes an organisation's map of its AI
estate and the credentials agents use to report into it. That is genuinely
sensitive — it is a target list — but it is not conversation data, and a report
premised on prompt or user-data exposure is describing something the system
cannot do.
