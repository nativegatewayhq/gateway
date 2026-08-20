# Gateway Agent Instructions

Before changing code or documentation, read these files in order:

1. `CONTRIBUTING.md`
2. `plans/README.md`
3. every plan relevant to the requested work

## Required behavior

- Do not implement meaningful work without an `accepted` or `in_progress` plan.
- Create new plans from `plans/TEMPLATE.md`.
- Keep implementation inside the accepted scope and exclusions.
- Use a new `change` plan for material design or acceptance-criteria changes.
- Run every verification command named by the plan.
- Record reproducible evidence before setting a plan to `completed`.
- Preserve native provider protocol behavior unless an accepted plan explicitly defines a conversion.
- Never log provider credentials, service API key plaintext, authorization headers, or unredacted secret-bearing URLs.
- Treat billing, idempotency, timeout, fallback, and reconciliation invariants as release blockers.
