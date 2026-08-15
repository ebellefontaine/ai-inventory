# AI Inventory — Design

**Date:** 2026-08-15
**Status:** Approved for phase 1 implementation
**Companion document:** [FUNCTIONAL.md](../../FUNCTIONAL.md)

---

## 1. What we are building

A single pane for every AI agent in a business — its function, owner, platform, hosting location, and eventually its connections. Agents self-register through an HTTP API or an MCP server. Agents that never register get found later by platform connectors.

Open source with self-hosting as the primary distribution, and a managed service later built from the same binary.

**Phase 1 (this design):** self-registration and inventory.
**Phase 2:** platform discovery and the connection graph.
**Phase 3:** metrics rollups.

---

## 2. Decisions

Five decisions were settled during design. Each is recorded with what was rejected, because the rejected options are the ones that come back.

### 2.1 No metric storage in phase 1

AI Inventory stores typed **links** to wherever telemetry already lives — a Datadog dashboard, a Langfuse project, a CloudWatch log group — and stores no metrics of its own.

*Rejected:* ingesting pre-aggregated rollups on the heartbeat (deferred to phase 3, and the heartbeat is shaped so it can be added without a breaking change); ingesting per-invocation events (rejected outright — it makes the client's non-impact guarantee far harder and puts the product in direct competition with dedicated observability tools).

### 2.2 Scoped enrollment tokens

An administrator mints a token scoped to one owning team and one environment. An agent registers with it and receives a per-agent credential for subsequent heartbeats and updates.

The consequence that matters: **owning team and environment are inherited from the token, not accepted from the descriptor.** An agent cannot misreport who owns it. Without this, "a complete list for auditors" degrades into "a list of whatever claimed to exist."

*Rejected:* open registration with a curation queue (invites spoofing, and every view needs a trust column to stay honest); OIDC workload identity federation (strongest attestation, but excludes laptops, n8n, and anything without a token issuer — revisit in phase 2 as an additional credential type, not a replacement).

### 2.3 Go, Postgres, one binary

A single static binary with the React SPA compiled in via `embed.FS`. Postgres is the only dependency. Self-host is `docker run` or a file plus a connection string. The managed service is the same binary with different configuration.

The reasoning weighted self-host distribution over build velocity, because distribution is the expensive thing to change later and the SDKs must be written in Python and TypeScript regardless of the server's language.

*Rejected:* a TypeScript monorepo (fastest to build and shares types end to end, but a Node runtime and its dependency surface are a harder sell to a security team reviewing a self-hosted install); Python with FastAPI (closest to the agent ecosystem, but still two languages because of the UI, and the worst self-host packaging story of the three).

### 2.4 Connections deferred entirely to phase 2

Phase 1 stores no connection data — not as a graph, and not as tagged strings.

This was deliberate. Tagged strings were rejected specifically *because* they are cheap: they accumulate free text that must later be reverse-engineered into nodes, which is the normaliser problem plus a backfill with no clean automated path. Storing nothing leaves phase 2 free to build the resource graph correctly.

When it lands, the model is canonical resource nodes with typed edges (`reads`, `writes`, `invokes`, `delegates_to`) and a provenance field distinguishing `declared` from `observed`. See FUNCTIONAL.md §10.

### 2.5 Record and flag; never reject

A token used from an unexpected environment, a descriptor missing fields, a registration that looks duplicated — all get written with a flag rather than rejected.

An inventory that rejects bad input develops blind spots, and blind spots are the exact failure the product exists to eliminate. This principle is why `stale`, `unclaimed`, and `flagged` are first-class states rather than error handling, and why nothing is ever deleted automatically.

---

## 3. Architecture

One process, four responsibilities:

- **HTTP API** — three agent-facing endpoints, plus the admin and UI API
- **MCP server** — streamable HTTP transport, exposing the same operations as tools
- **Web UI** — React SPA embedded in the binary
- **Background workers** — lifecycle transitions in phase 1, connectors in phase 2

The HTTP and MCP surfaces are thin adapters over one service layer. Neither contains business logic. This is enforced by running one contract test suite against both.

Every table carries an organisation ID and every query is scoped to it, even though a self-hosted install has exactly one organisation. Retrofitting tenancy is a rewrite; carrying an unused column is free.

---

## 4. Agent identity

The decision that most determines whether the product works at all.

An agent redeploys, restarts, and scales to twenty replicas. If each event creates a row, the inventory is worthless within a week.

**Agents are keyed on `(organisation, environment, instance_key)`, and registration is an idempotent upsert on that key.** Twenty replicas collapse into one record carrying a replica count.

The SDK derives `instance_key` in priority order:

1. Explicit configuration
2. Deployment identity — Lambda function ARN, Kubernetes deployment name, ECS service ARN, Cloudflare Worker name
3. Hash of source repository URL plus entrypoint

Idempotency is also what makes the low-latency requirement achievable. If a retry costs nothing, the client can be dumb, asynchronous, and fire-and-forget.

**Known gap:** serverless platforms make deployment identity ambiguous once versions and aliases are involved. Needs a per-platform decision before the SDK ships — tracked in FUNCTIONAL.md §14.

---

## 5. Data model

```
organizations       id, name, slug

teams               id, org_id, name, slug, owner_emails[]

enrollment_tokens   id, org_id, team_id, environment, name,
                    token_hash, expires_at, revoked_at,
                    created_by, last_used_at

agents              id, org_id, instance_key, display_name, purpose,
                    team_id, environment, lifecycle_state,
                    platform{framework, framework_version},
                    hosting{provider, region, account_id, resource_id},
                    source{repo_url, commit_sha},
                    telemetry_links[]{kind, label, url, external_id},
                    registration_source, replica_count, flags[],
                    first_seen_at, last_heartbeat_at, updated_at,
                    attributes(jsonb)

agent_revisions     id, agent_id, snapshot(jsonb), actor, changed_at
                    -- append-only

agent_credentials   id, agent_id, token_hash, issued_at,
                    expires_at, revoked_at

audit_log           id, org_id, actor, action, target_type, target_id,
                    before(jsonb), after(jsonb), at
                    -- append-only

users, roles        OIDC subject, org_id, role
```

Notes on specific choices:

**`agent_revisions` is append-only and written on every accepted change.** "What did this look like in March" is a question auditors actually ask. It is nearly free to capture at write time and impossible to reconstruct afterwards.

**`telemetry_links` is typed, not a blob.** Kind, label, URL, and external ID mean the UI can render a Datadog link differently from a Langfuse one, and the export can be validated.

**`attributes` is a jsonb escape hatch.** Unrecognised descriptor fields land here rather than being dropped — a direct consequence of §2.5.

**`flags[]` carries the record-and-flag outcomes**: environment mismatch, validation failure, credential revoked, missing required fields.

**No `model_ids` field.** Models are connections, and connections are deferred. This was raised explicitly and confirmed. It is the most likely candidate for scope creep back into phase 1.

---

## 6. Agent-facing API

Three endpoints. This is the entire surface an agent touches.

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /api/v1/agents:register` | enrollment token | Idempotent upsert. Returns agent ID, per-agent credential, heartbeat interval. |
| `POST /api/v1/agents/{id}/heartbeat` | agent credential | Liveness and replica count. ~200-byte body. |
| `PATCH /api/v1/agents/{id}` | agent credential | Partial descriptor update. Writes a revision. |

**The heartbeat interval is returned by the server, not hardcoded in the client.** It is the only operational lever available when five thousand agents are hammering a struggling instance and redeploying them is not an option.

### A2A interoperability

The registration body accepts an optional `agent_card` field conforming to the A2A Agent Card. The A2A card defines `name`, `description`, `version`, `provider`, `skills[]`, `documentationUrl`, and interface and security declarations — and defines nothing about owning team, hosting location, environment, or lifecycle.

The two are complementary rather than overlapping. Mapping the overlapping fields costs almost nothing and buys interoperability with anything already publishing a card.

### MCP surface

`register_agent`, `heartbeat`, and `update_agent` mirror the HTTP endpoints; `get_agent` and `search_agents` are read tools.

This is worth more than protocol box-ticking. An agent that already speaks MCP needs **zero** new dependency — add a server to its configuration and the model itself performs the registration. That is a stronger answer to "the smallest amount of code" than any SDK can offer.

---

## 7. SDK

```python
from aiinventory import register

register(name="invoice-triage", purpose="Triages AP invoices")
```

Token and service URL come from `AI_INVENTORY_TOKEN` and `AI_INVENTORY_URL`. Everything else is detected:

| Detected | Source |
|---|---|
| Framework and version | Module inspection — LangGraph, CrewAI, LangChain, Claude Agent SDK |
| Hosting | Cloud metadata endpoints and platform env vars — `AWS_LAMBDA_FUNCTION_NAME`, `ECS_CONTAINER_METADATA_URI`, Kubernetes downward API, `WEBSITE_SITE_NAME` |
| Source | Git metadata, or CI env vars such as `GITHUB_SHA` |

Every detected value is overridable. Detection failure degrades to `unknown`; it never raises.

### The non-impact guarantee

This is the requirement most likely to regress silently, so it is specified as mechanism rather than intent:

- `register()` enqueues onto a bounded channel and returns. A single background worker owns all network I/O.
- The queue drops oldest-first when full. It never blocks the caller and never grows without bound.
- **No client code path raises into the caller.** Failures log at warning level and nothing more.
- Retries use exponential backoff with full jitter. A circuit breaker backs off to a long interval after repeated consecutive failures.
- Steady state is one small request per heartbeat interval.

---

## 8. Failure modes

| Situation | Behaviour |
|---|---|
| Service unreachable | Agent unaffected. Client backs off, re-registers on recovery — safe because registration is idempotent. |
| Enrollment token revoked | Its agents are flagged `credential_revoked`, not deleted. Deletion would destroy the audit history that justifies the product. |
| Agent stops heartbeating | `idle` after a grace period, `stale` after a longer configurable threshold. Never auto-deleted — `stale` is itself an audit finding. |
| Token used from an unexpected environment | Registration succeeds, flag raised. Rejecting would push shadow AI back into invisibility. |
| Replicas registering concurrently | Idempotent upsert converges to one record. No error surfaces to any caller. |
| Database outage | Writes fail, service does not crash. Read views degrade to last-known state. |

---

## 9. Humans, UI, and access

OIDC against the organisation's existing identity provider, plus one bootstrap local administrator. **No password management** — every enterprise has an identity provider, and building user management is a tarpit that adds no product value.

Roles: administrator, editor, viewer, auditor. Auditor is read plus export with no mutation capability.

The phase 1 UI is deliberately small:

- Agent list, filterable on team, environment, platform, hosting provider, lifecycle state, and last-seen
- Agent detail with revision history and telemetry deep-links
- Gaps view — unclaimed, stale, flagged, incomplete
- Team and token management
- Filtered CSV and JSON export

The export is the literal deliverable of the CIO story, so its shape is covered by golden-file tests. An auditor artifact whose columns shift between releases is worse than no artifact.

---

## 10. Testing

| Area | Approach |
|---|---|
| API contract | One golden fixture set, executed against **both** the HTTP and MCP surfaces, so they cannot drift. |
| SDK conformance | One shared specification that the Python and TypeScript SDKs must both pass, run against a real server in a container. |
| Persistence | Postgres via testcontainers, not mocks. Concurrent upsert-on-`instance_key` is precisely the behaviour mocks hide. |
| Non-impact (NFR-1) | Run a host agent loop while the server returns 500s, hangs, and drops TCP mid-request. Assert the loop's p99 is statistically unchanged versus a run with no client installed. |
| Export | Golden-file tests on CSV and JSON output. |
| Lifecycle | Time-controlled tests for the idle and stale transitions. |

NFR-1 is tested rather than asserted because it is a promise to every engineer who installs the SDK, and a regression would be invisible until it had already damaged trust in the product.

---

## 11. Explicitly out of phase 1

Connections and the resource graph. Metrics ingestion. Discovery connectors. Graph visualisation. Agent-to-agent topology. SCIM provisioning. Alerting.

And out of the product entirely, per FUNCTIONAL.md §13: prompt or completion capture, agent execution or orchestration, model evaluation, general-purpose observability, runtime policy enforcement.

---

## 12. Next step

An implementation plan derived from this design, sequencing phase 1 into reviewable increments.
