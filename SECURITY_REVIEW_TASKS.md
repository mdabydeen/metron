# Security Review Remediation Tasks

Derived from `SECURITY_REVIEW_PLAN.md` and tracked through integration:

- [x] P1: Enforce the project-configuration trust boundary; preserve safe project settings, explicit `METRON_CONFIG`, user configuration, and environment precedence.
- [x] P2: Emit capability warnings on startup and in `--doctor`, including ignored project privilege requests.
- [x] P3: Make one-shot `-p/--prompt` fail closed unless `--yes` is explicitly supplied.
- [x] P4: Validate patch targets from edit, create, delete, rename, copy, traversal, absolute-path, and symlink cases.
- [x] P5: Reject cross-host or unsafe redirects and document command/environment/gitignored-file risks plus residual TOCTOU risk.
- [x] Verification: tracked-source formatting, static analysis, unit tests, integration tests, race tests, and 100% statement coverage pass.
- [x] Delivery: commit the completed remediation and create pull request #15.

This checklist is intentionally separate from the review plan so the plan remains
the security requirements while this file records implementation progress.
