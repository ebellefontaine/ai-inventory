# AI Inventory

A single pane for every AI agent operating inside a business — what it does, who owns it, what it was built on, and where it runs.

> **Status: phase 1 in development.** The repository is scaffolding today. The
> quickstart and SDK examples below describe the intended interface, not
> working software — nothing in this README runs yet. Follow
> [SYSTEM.md](SYSTEM.md) for what is committed to and
> [FUNCTIONAL.md](FUNCTIONAL.md) for the requirements being built against.

## The problem

Most organisations passed the point of knowing what agents they run somewhere around the second quarter someone shipped one without telling anyone. Those agents are real, they touch production data, they cost money, and there is no list. When an auditor, a regulator, or a board member asks what AI is running in the company and who is accountable for it, the honest answer today is usually a spreadsheet that was accurate last month.

## Quickstart

**1. Run it.** One binary, one database.

```bash
cp .env.example .env
docker compose up -d
```

The UI and API come up on <http://localhost:8080>. Schema migrations, the
organisation, and the bootstrap administrator initialise on first run.

> Today `docker-compose.yml` defines only PostgreSQL. The `aiinv` service joins
> it once there is a binary worth running.

**2. Mint an enrollment token.** Tokens are scoped to exactly one team and one
environment. Agents inherit ownership from the token rather than claiming it in
the payload, which is what makes the inventory defensible rather than
self-reported.

```bash
aiinv token create --team finance-platform --env production
```

The token value is shown exactly once.

**3. Register an agent.**

```bash
curl -X POST http://localhost:8080/api/v1/agents:register \
  -H "Authorization: Bearer $AIINV_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "invoice-triage", "description": "Triages accounts-payable invoices"}'
```

## From an agent

Three lines. Framework, version, hosting provider, region, repository, and
commit are auto-detected from the runtime; `AIINV_URL` and `AIINV_TOKEN` are
read from the environment.

```python
from aiinv import register

register(name="invoice-triage", description="Triages accounts-payable invoices")
```

TypeScript ships alongside Python, and both pass one shared conformance suite
run against a real server. Agents that already speak MCP register by adding a
server to their configuration, with no code at all.

Registration never blocks or breaks the agent, and AI Inventory never collects
prompts, completions, or end-user data. See [SYSTEM.md §7](SYSTEM.md).

## Documentation

- **[SYSTEM.md](SYSTEM.md)** — what the product is, how it is built, and what it deliberately is not.
- **[FUNCTIONAL.md](FUNCTIONAL.md)** — user stories and acceptance criteria.
- **[docs/plans/](docs/plans/)** — design doc and implementation plan.

## License

| Component | License |
|---|---|
| Server, UI, connectors | [AGPL-3.0](LICENSE) |
| Python and TypeScript SDKs, MCP client | [Apache-2.0](sdk/python/LICENSE) |

The split is deliberate. AGPL means a competitor cannot run this as a hosted
service without publishing their changes. Apache-2.0 on the SDKs means nobody
has to think twice about embedding a client in a production agent. See
[sdk/README.md](sdk/README.md).

## Contributing

Contributions are welcome. Sign-off is DCO (`git commit -s`), not a CLA. See
[CONTRIBUTING.md](CONTRIBUTING.md) to get set up, and
[SECURITY.md](SECURITY.md) to report a vulnerability.
