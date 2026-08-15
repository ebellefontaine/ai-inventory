# AI Inventory — Functional Requirements

**Status:** Draft
**Last updated:** 2026-08-15

---

## 1. Purpose

AI Inventory is a single pane for every AI agent operating inside a business. For each agent it answers five questions:

1. **What does it do?** — its function and purpose
2. **Who owns it?** — the accountable team and people
3. **What was it built on?** — framework, model, version, source repo
4. **Where does it run?** — cloud, region, account, hosting platform
5. **What does it touch?** — datastores, APIs, MCP servers, other agents *(phase 2)*

Agents populate the inventory by self-registering. Agents that never register are found by integrations that poll the platforms they run on.

The product exists to eliminate blind spots. Every design decision resolves in favour of *knowing that something exists* over *knowing everything about it*.

---

## 2. Scope and phasing

| Phase | Theme | Status |
|---|---|---|
| **Phase 1** | Self-registration and inventory | PoC scope — build this |
| **Phase 2** | Platform discovery and the connection graph | Specified, not built |
| **Phase 3** | Metrics and cost rollups, developer-tooling visibility | Directional only |

Every story below is tagged with its phase. Phase 2 and 3 stories are documented now so that the phase 1 data model does not foreclose them — not because they are in the PoC.

---

## 3. Definitions

Terms used precisely throughout this document and in the code.

| Term | Meaning |
|---|---|
| **Agent** | A distinct, deployed AI workload with a stable identity. Not a process, not a request, not a conversation. |
| **Instance key** | A stable, client-supplied identifier for an agent, unique within an organisation and environment. Twenty replicas of one deployment share one instance key and appear as one agent. |
| **Replica** | A running copy of an agent. Counted, never enumerated as separate agents. |
| **Descriptor** | The self-reported record an agent submits: name, purpose, platform, hosting, source, telemetry links. |
| **Enrollment token** | A credential minted by an administrator, scoped to one owning team and one environment. Used once per agent to register. |
| **Agent credential** | A per-agent token issued in the registration response, used for heartbeats and descriptor updates. |
| **Telemetry link** | A typed pointer to where an agent's metrics already live (Datadog dashboard, Langfuse project, CloudWatch log group). AI Inventory stores the pointer, not the metrics. |
| **Lifecycle state** | `active`, `idle`, `stale`, `retired`, or `quarantined`. |
| **Registration source** | `self_registered`, `discovered`, or `manual`. |
| **Unclaimed** | An agent record with no owning team. Discovered agents start here. |
| **Resource** *(P2)* | A canonical node an agent connects to — a datastore, model, MCP server, SaaS API, or another agent. |
| **Edge** *(P2)* | A typed link from an agent to a resource: `reads`, `writes`, `invokes`, `delegates_to`. |
| **Provenance** *(P2)* | Whether a fact is `declared` (claimed or inferred) or `observed` (proven at runtime). |
| **Connector** *(P2)* | An integration that polls a hosting platform and reports the agents it finds. |

---

## 4. Personas

| Persona | Who they are | What they need |
|---|---|---|
| **AI Agent** | A non-human actor. Registers and reports on itself. | To register cheaply and never be slowed down by doing so. |
| **AI Engineer** | Builds and ships agents. | To add inventory support in minutes, not days, and then forget about it. |
| **Service Owner** | Runs AI Inventory for the business. Platform or AI enablement team. | Coverage. To know what exists, including what hasn't registered. |
| **CIO / Compliance** | Accountable for AI governance. Faces auditors. | A complete, defensible, exportable list — and an honest account of its gaps. |

---

## 5. Phase 1 — Agent self-registration

### AR-1 — Register via HTTP API `P1`

> As an AI Agent, I want to register myself with AI Inventory using a single API call.

**Acceptance criteria**
- `POST /api/v1/agents:register` accepts an enrollment token in the `Authorization` header and a descriptor in the body.
- The response contains the assigned agent ID, a per-agent credential, and the heartbeat interval the server wants this agent to use.
- Registration succeeds with only `name` and `purpose` supplied; every other descriptor field is optional.
- Registration completes in a single round trip. No handshake, no pre-provisioning, no prior knowledge of the agent on the server.

### AR-2 — Register via MCP `P1`

> As an AI Agent, I want to register myself using an MCP server, without adding a code dependency.

**Acceptance criteria**
- An MCP server exposes `register_agent`, `heartbeat`, and `update_agent` tools with the same semantics as the HTTP endpoints.
- Read tools `get_agent` and `search_agents` are available to authenticated callers.
- Both surfaces delegate to one service layer, and the same contract test suite runs against both. The MCP server is never a second implementation.
- An agent that already speaks MCP can be inventoried by adding a server to its configuration and nothing else.

### AR-3 — Report identity, ownership, and location `P1`

> As an AI Agent, I want to share what I am running, who built and owns me, and where I am running.

**Acceptance criteria**
- The descriptor carries: display name, purpose, framework and framework version, hosting provider, region, account identifier, hosting resource identifier, source repository URL, and commit SHA.
- Owning team and environment are **not** accepted from the descriptor. They are inherited from the enrollment token's scope, so an agent cannot misreport who owns it.
- Unrecognised descriptor fields are preserved in an attributes bag rather than rejected.
- An optional `agent_card` field accepts an A2A Agent Card; `name`, `description`, `version`, `provider`, and `skills` are mapped onto the descriptor.

### AR-4 — Point at existing telemetry `P1`

> As an AI Agent, I want to tell the inventory where my performance and cost data already lives.

**Acceptance criteria**
- The descriptor accepts a list of typed telemetry links, each with a kind, label, URL, and external identifier.
- Recognised kinds render as first-class links in the UI; unrecognised kinds render generically rather than being dropped.
- AI Inventory stores **no metrics of its own** in phase 1. See MET-1 for the phase 3 successor.

### AR-5 — Re-register safely `P1`

> As an AI Agent, I want restarting, redeploying, or scaling out to not corrupt the inventory.

**Acceptance criteria**
- Registration is an idempotent upsert keyed on `(organisation, environment, instance_key)`.
- N replicas of one deployment produce one agent record with a replica count, not N records.
- Re-registering with a changed descriptor updates the record and writes a revision; it does not create a duplicate.
- Concurrent registrations from replicas starting simultaneously converge to one record with no error surfaced to any caller.

### AR-6 — Heartbeat `P1`

> As an AI Agent, I want to signal that I am still alive, cheaply.

**Acceptance criteria**
- `POST /api/v1/agents/{id}/heartbeat` accepts the per-agent credential and a body of roughly 200 bytes.
- The heartbeat interval is dictated by the server in the registration response and can be changed fleet-wide without redeploying agents.
- A heartbeat updates last-seen and replica count only. It is not a descriptor update.

### AR-7 — Update the descriptor `P1`

> As an AI Agent, I want to report changes when what I am or where I run changes.

**Acceptance criteria**
- `PATCH /api/v1/agents/{id}` accepts partial descriptor updates using the per-agent credential.
- Every accepted change writes a revision recording what changed, when, and which actor changed it.

### AR-8 — Never be slowed down `P1`

> As an AI Agent, I want my connection to AI Inventory to be low latency and non-impacting to my own performance and tasks.

**Acceptance criteria**
- The client's register and heartbeat calls return to the caller in microseconds. All network I/O happens on a background worker.
- The client's outbound queue is bounded and drops oldest-first when full. It never blocks the calling thread and never grows without limit.
- The client **never raises into the caller** under any failure condition. Failures are logged at warning level.
- Steady-state cost is one small request per heartbeat interval, per agent.
- See NFR-1 for the measurable form of this requirement.

### AR-9 — Survive an inventory outage `P1`

> As an AI Agent, I want an AI Inventory outage to be invisible to my work.

**Acceptance criteria**
- With the server unreachable, hanging, or returning errors, the agent's own workload is unaffected.
- Retries use exponential backoff with full jitter. After repeated consecutive failures a circuit breaker backs the client off to a long interval.
- On recovery the client re-registers automatically; idempotency makes this safe.

---

## 6. Phase 1 — AI Engineer

### ENG-1 — Minimum viable integration `P1`

> As an AI Engineer, I want to drop in the smallest amount of code needed for my agent to register and report.

**Acceptance criteria**
- The happy path is one import and one call, with the token and service URL read from environment variables.
- No configuration file is required.
- SDKs ship for Python and TypeScript, both passing one shared conformance specification executed against a real server.

### ENG-2 — Auto-detect the environment `P1`

> As an AI Engineer, I don't want to hand-write metadata that the runtime already knows.

**Acceptance criteria**
- The SDK detects framework and version by module inspection (LangGraph, CrewAI, LangChain, Claude Agent SDK, and others).
- The SDK detects hosting from cloud metadata and platform environment variables — AWS Lambda, ECS, EKS/Kubernetes, Azure App Service, Cloudflare Workers.
- The SDK detects source repository and commit from git metadata or CI environment variables.
- Every auto-detected value can be overridden explicitly. Detection failure degrades to `unknown`, never to an error.

### ENG-3 — Verify it worked `P1`

> As an AI Engineer, I want to confirm my agent registered without opening the UI.

**Acceptance criteria**
- The SDK exposes a synchronous, opt-in verification call for use in tests and CI that returns the server's view of the agent.
- Enabling debug logging surfaces what was detected, what was sent, and what the server returned.

### ENG-4 — Keep local development out of the production inventory `P1`

> As an AI Engineer, I want my laptop runs not to pollute the real inventory.

**Acceptance criteria**
- Enrollment tokens are scoped to an environment; a development token cannot produce production records.
- The UI and exports filter by environment by default.
- Records created by development tokens are subject to a shorter stale threshold.

---

## 7. Phase 1 — Service Owner

### OWN-1 — Mint scoped enrollment tokens `P1`

> As a Service Owner, I want to hand teams a credential that carries their identity, so ownership is inherited rather than self-asserted.

**Acceptance criteria**
- An administrator creates a token scoped to exactly one team and one environment, with an optional expiry.
- The token value is displayed once at creation and stored only as a hash.
- Token list shows name, scope, creation, expiry, and last use.

### OWN-2 — Revoke a token without destroying history `P1`

**Acceptance criteria**
- Revoking a token immediately prevents further registrations with it.
- Agents registered with a revoked token are flagged `credential_revoked`. They are **not** deleted — deletion would destroy the audit history that justifies the product.
- Revocation is recorded in the audit log with actor and time.

### OWN-3 — Find the gaps `P1`

> As a Service Owner, I want to see what the inventory is unsure about.

**Acceptance criteria**
- A view lists agents that are unclaimed, stale, flagged, or missing required descriptor fields.
- Each entry states plainly why it is listed.

### OWN-4 — Correct the record `P1`

**Acceptance criteria**
- An administrator can reassign an agent's owning team, change its lifecycle state, and edit descriptor fields.
- Human edits are marked as such and are not overwritten by subsequent self-reports unless explicitly released back to auto-management.
- Every edit writes a revision and an audit log entry.

### OWN-5 — Record and flag rather than reject `P1`

> As a Service Owner, I want questionable data recorded and flagged, not thrown away.

**Acceptance criteria**
- A token used from an unexpected hosting environment records the agent and raises a flag; it does not reject the registration.
- Malformed but parseable descriptors are stored with a validation flag.
- Only genuinely unauthenticated or unparseable requests are rejected.

### OWN-6 — Lifecycle transitions happen automatically `P1`

**Acceptance criteria**
- An agent that misses heartbeats past a grace period becomes `idle`; past a configurable longer threshold it becomes `stale`.
- Records are never automatically deleted. `stale` is itself a reportable finding.
- `retired` and `quarantined` are set by humans only.

### OWN-7 — Self-host without ceremony `P1`

**Acceptance criteria**
- Deployment is a single binary plus a Postgres connection string, or one container image.
- The web UI is served by the same process; there is no separate frontend deployment.
- The organisation, admin bootstrap, and schema migrations initialise on first run.

---

## 8. Phase 1 — CIO, Compliance, and Audit

### AUD-1 — Produce a complete list `P1`

> As a CIO, I want to know what agents are running where, and give auditors a complete list.

**Acceptance criteria**
- One view lists every known agent with function, owner, platform, hosting location, environment, lifecycle state, and last-seen.
- The list is filterable on every one of those dimensions.
- The list distinguishes self-registered, discovered, and manually entered records.

### AUD-2 — Export it `P1`

**Acceptance criteria**
- Any filtered view exports to CSV and JSON.
- The export records the filter applied, the generating user, and the generation timestamp.
- Export shape is stable across releases and covered by golden-file tests.

### AUD-3 — Answer questions about the past `P1`

**Acceptance criteria**
- Every agent has a revision history showing what changed, when, and by which actor.
- An agent detail view can be rendered as of a past date.
- The audit log is append-only and covers token, ownership, lifecycle, and descriptor changes.

### AUD-4 — Be honest about coverage `P1`

> As a CIO, I want to know what the inventory does **not** know, because an overstated list is worse than a short one.

**Acceptance criteria**
- Every export and summary view states the counts of unclaimed, stale, and flagged records alongside the total.
- Fields that were auto-detected, human-edited, or defaulted are distinguishable from fields the agent explicitly reported.
- The product never presents an inferred value as a reported one.

### AUD-5 — Read-only access `P1`

**Acceptance criteria**
- Human authentication is OIDC against the organisation's existing identity provider, plus one bootstrap local administrator.
- Roles are administrator, editor, viewer, and auditor. Auditor is read plus export, with no mutation capability.
- No password management is built into the product.

---

## 9. Phase 2 — Discovery

> As a Service Owner, I want agents discovered through connections to the services that host them, so agents that never register still appear.

Each connector implements one interface: given credentials, return the agents it can see. Results reconcile against existing records by instance key or platform-native identifier. Anything that fails to match becomes an unclaimed record awaiting a human owner.

### DISC-1 — Connector framework `P2`
- Connectors are scheduled, run independently, and one failing connector never blocks another.
- Every discovered fact records which connector produced it and when.
- A connector's credentials are scoped read-only wherever the platform allows it.

### DISC-2 — Reconciliation `P2`
- A discovered agent that matches a self-registered one merges into the existing record rather than duplicating it.
- Conflicting values between a self-report and a discovery are both retained; the self-report wins for display, and the conflict is flagged.
- Unmatched discoveries become `unclaimed` records in a triage queue.

### DISC-3 — Target platforms `P2`

Priority order, subject to revision:

| Platform | Notes |
|---|---|
| AWS Bedrock Agents | Highest expected enterprise density |
| Azure AI Foundry | |
| Claude managed agents | |
| n8n | Frequent home of unmanaged automation |
| Cloudflare Agents / Workers | |
| Notion agents | |
| LangGraph Platform | |
| Custom / self-hosted | Falls back to self-registration |

### DISC-4 — Claiming `P2`
- An administrator can assign an owning team to an unclaimed agent, or mark it as out of scope with a reason.
- Out-of-scope records remain in the inventory and remain exportable.

---

## 10. Phase 2 — Connections

> As a CIO, I want to know what each agent is connected to, and to run that question backwards.

Deferred from the PoC. The phase 1 schema must not foreclose it.

### CONN-1 — Resources as first-class nodes `P2`
- Connections resolve to canonical resource records — datastore, model, MCP server, SaaS API, or another agent.
- A deterministic per-type normaliser canonicalises typed input. Resources carry aliases so a manual merge is a durable one-time correction.
- Anything that does not canonicalise confidently becomes a low-confidence resource in a review queue rather than a silent duplicate node.

### CONN-2 — Typed edges `P2`
- Edges carry a type: `reads`, `writes`, `invokes`, `delegates_to`.
- "Which agents can write to this datastore" is answerable separately from "which can read it".

### CONN-3 — Provenance `P2`
- Every edge records whether it is `declared` or `observed`, and which connector or SDK produced it.
- The UI never renders a declared edge as though it were proven.

### CONN-4 — Reverse lookup `P2`
- Given a resource, list every agent connected to it, with edge types.
- Given an agent, list everything it can reach.
- Third-party destinations are distinguishable from internal ones.

---

## 11. Phase 3 — Directional

### MET-1 — Metrics and cost rollups `P3`
- Agents report pre-aggregated counters on the heartbeat: invocations, tokens, estimated cost, error rate, latency percentiles, bucketed hourly or daily.
- Never per-invocation events. The product does not compete with dedicated observability platforms.
- This extends the phase 1 heartbeat rather than replacing it.

### DEV-1 — Developer tooling visibility `P3`
- Visibility into scheduled and recurring agents running in Cowork, Claude Code, Warp, and similar tooling.
- Explicitly nice-to-have. Personal developer activity raises consent and privacy questions that must be settled before any of this is built.

---

## 12. Non-functional requirements

### NFR-1 — The client cannot harm the agent
The single hardest requirement in the product, and the one most likely to regress silently.

- Register and heartbeat calls return to the caller in under one millisecond at p99.
- With the server returning errors, hanging, or dropping connections mid-request, a host agent loop's p99 latency is statistically unchanged from a run with no client installed. This is a **test**, not an assertion.
- The client's memory footprint is bounded and does not grow with uptime or failure duration.
- No client code path blocks on network I/O from the caller's thread.

### NFR-2 — Server performance
- Registration responds in under 100 ms at p95, excluding network.
- Heartbeat responds in under 30 ms at p95.
- A single modest instance sustains 10,000 registered agents on a 60-second heartbeat.

### NFR-3 — Degradation
- A database outage fails writes but does not crash the service.
- Read views degrade to last-known state rather than erroring.
- No single connector failure affects registration or the UI.

### NFR-4 — Security
- Tokens are stored as hashes only and displayed exactly once.
- All credentials are scoped, expirable, and revocable.
- Every mutation is attributable to an actor in the audit log.
- Multi-tenancy is enforced at the data layer from day one, even though self-host has one organisation.

### NFR-5 — Privacy boundary
A hard product boundary, not a configuration option.

- AI Inventory **never** collects prompts, completions, conversation content, end-user data, or model inputs and outputs.
- It collects metadata about agents: what they are, who owns them, where they run.
- No SDK code path can transmit user data, by construction. This is what makes the product deployable in regulated environments.

### NFR-6 — Retention
- Agent records are never automatically deleted.
- Revision history and audit log retention are configurable, with a floor appropriate to audit requirements.
- Hard deletion is an explicit administrator action, itself audited.

### NFR-7 — Operability
- One binary, one database. Postgres is the only required dependency.
- Schema migrations run automatically on start and are reversible.
- Configuration by environment variable, with sane defaults for everything except the database URL.

---

## 13. Out of scope

Not in the product, in any phase, unless this document changes:

- Prompt, completion, or conversation capture — see NFR-5
- Agent execution, orchestration, or scheduling
- Model evaluation or quality scoring
- Being a general-purpose observability or APM platform
- Policy enforcement or runtime blocking of agents
- A CMDB for non-AI infrastructure

---

## 14. Open questions

1. **Instance key derivation for serverless.** Lambda and Workers make deployment identity ambiguous when versions and aliases are in play. Needs a decision per platform.
2. **Environment inference conflicts.** When a production-scoped token registers from a detected development environment, is the flag enough or does it need to alert?
3. **Multi-team agents.** Some agents genuinely have shared ownership. Does the model need co-owners, or is one accountable team plus a contacts list sufficient?
4. **Discovery credential model.** Read-only cross-account roles per cloud is the obvious answer for AWS and Azure; less obvious for SaaS platforms with coarse API tokens.
5. **Consent for DEV-1.** Inventorying agents a developer runs on their own machine needs an answer to "who agreed to this" before any code is written.
