# Issue triage guide

This repository uses GitHub issue forms as the intake contract for roadmap work.
The forms collect enough evidence for deduplication, project routing, and safe
automation decisions; maintainers still own final labels and project fields.

## Project field vocabulary

Use these values for the `atteler roadmap` project (`tomMoulard/3`). The source
of truth checked into the repository is `.github/project-fields.yml`, and the
issue-template test fails if forms or this guide drift from it.

| Field | Supported values | Default for new issues |
| --- | --- | --- |
| Status | Todo, In Progress, Done | Todo |
| Priority | P0 - Now, P1 - Next, P2 - Later | P1 - Next unless impact clearly warrants P0 or P2 |
| Area | Roadmap, CLI/UX, Scanner, RAG, Review Gates, Workflow Audit, Providers, Permissions, Worktree, SDK | Use the form answer; Roadmap only for intake/project-process work |
| Risk | Trust/Safety, Product Coherence, Architecture Debt, Quality Signal | Use the form answer; prefer Quality Signal for test/check failures |

The forms request `tomMoulard/3` project membership. If GitHub cannot auto-add
the issue to the project because of caller permissions or ProjectV2 settings,
add it manually before applying the field defaults above.

## Label triage

Start from the template labels, then adjust only when the submitted evidence
supports it:

- `bug`: reproducible broken behavior or regression.
- `enhancement`: new behavior, provider support, or documentation improvement.
- `documentation`: README, guide, help text, or generated-doc work.
- `quality`: tests, verification, audits, and quality-gate findings.
- `roadmap`: project/planning items that should remain visible on the roadmap.
- `provider`: provider/model integration, auth, capability, pricing, or catalog work.
- `security`: public-safe trust, permission, safety, or hardening concerns.
- `ux`: CLI/TUI discoverability or interaction improvements.
- `rag`: retrieval, memory, embeddings, indexes, and related docs.
- `architecture` / `debt`: package-boundary, maintainability, and migration work.
- `symphony`: explicitly automation-dispatchable work only.

## Roadmap membership vs. Symphony dispatch

Adding an issue to the `atteler roadmap` project or applying `roadmap` means the
work is tracked and prioritized. This does not request or authorize automation.

Apply `symphony` only when all of the following are true:

1. The issue came from the Symphony-dispatchable task form, or the author later
   made an explicit automation opt-in in the issue discussion.
2. The task is public-safe: no secrets, credentials, private data, or unsafe
   exploit details are present.
3. Scope, evidence, acceptance criteria, and verification commands are concrete
   enough for a coding agent to complete and prove the work.
4. A maintainer agrees the current project fields and labels describe the task.

For ordinary bug, feature, audit, provider, or security-sensitive forms, keep the
issue human-triaged unless a maintainer adds `symphony` after explicit opt-in.

## Security-sensitive reports

Public issues are acceptable for design-level trust, permissions, and hardening
concerns that do not disclose private or exploitable details. Direct reports
with exploit steps, leaked secrets, token material, or private user data should
go to GitHub Private Vulnerability Reporting instead:

<https://github.com/tomMoulard/atteler/security/advisories/new>
