---
name: simplify
description: "Simplify a completed workpackage without changing its intended behavior: remove needless abstraction, duplication, indirection, speculative flexibility, and defensive code, then report what changed and what was left for approval. Use when the user says \"/simplify\", asks to simplify a just-finished change, or wants a cleanup pass over a branch, PR, or set of files."
argument-hint: "The commit, branch, PR, or files to simplify; defaults to this branch's changes"
disable-model-invocation: true
---

# Simplify

Review the completed workpackage and simplify its implementation without changing its intended behavior.

## 1. Establish the workpackage

Identify the code under review before touching anything. In order of preference:

- The fixed point the user named (commit, branch, tag, base branch, `HEAD~N`).
- The default branch's merge-base with the current branch: `git diff "$(git merge-base origin/HEAD HEAD)..HEAD"` (fall back to `origin/main` or `origin/master` when `origin/HEAD` is unset), plus `git log --oneline <fixed-point>..HEAD` for intent.
- The files, module, or directory the user named.
- Uncommitted work, when the change is not committed yet.

If none of these resolve to a concrete change, ask which change is the workpackage. Read the commit messages and any linked issue or spec: they define the intended behavior that simplification must preserve.

## Scope

- Focus on code added or affected by the workpackage and the nearby code needed to simplify it coherently.
- Do not perform unrelated cleanup.
- Read the affected files in full and understand the relevant invariants, call sites, tests, and public API before editing.
- Optimize for a direct, coherent, maintainable design—not the smallest diff.
- Names belong to the domain: when the codebase or its docs give a concept a specific term, keep that term rather than renaming it in passing.

## What to remove or simplify

- Remove abstractions that have no clear responsibility or do not reduce real complexity.
- Inline trivial helpers, wrappers, adapters, and pass-through layers when they obscure rather than clarify behavior.
- Remove duplicated logic and put shared behavior at the appropriate existing layer.
- Remove speculative flexibility, configuration, extension points, and generic machinery that have no current requirement.
- Remove speculative defensive programming inside trusted code. Validate at real trust boundaries; rely on types and established invariants internally.
- Do not keep fallback behavior for states that should be impossible. Prefer fixing the invariant or type model.
- Simplify excessive state, boolean flags, branching, indirection, and special cases. Rework the underlying representation when that is clearer.
- Tighten TypeScript types so invalid states are unrepresentable. Avoid `any`, unnecessary assertions, broad types, and optional fields for impossible states.
- Prefer direct, readable code over cleverness, premature abstraction, and framework-like infrastructure.
- Delete comments that merely restate the code, but preserve comments that explain constraints, intent, or non-obvious decisions.

Before proposing a replacement that introduces shared code, search outside the diff for the existing helper, type, or module that already owns that behavior. Match abstraction size to current needs: similar-looking code is not enough reason to couple modules.

## Approval gate for significant removals

Classify each candidate change before applying it. Routine, behavior-preserving cleanup proceeds without asking: inlining a single-use trivial helper, removing an unreachable branch proven impossible by the type model, deleting newly added duplication. If uncertain whether a removal is significant, ask.

Before making any significant or potentially intentional removal, stop and ask the user for approval. First explain:

1. what you propose to remove or change;
2. why it appears unnecessary or overly defensive;
3. what behavior, compatibility, extensibility, validation, or failure handling could be affected;
4. the simpler replacement, if any; and
5. your recommendation.

Wait for explicit approval before applying that change. Do not bundle approval for multiple independent decisions; present them separately when the tradeoffs differ.

Treat a decision as significant when it removes or materially changes any of the following:

- user-visible behavior or supported workflows;
- public APIs, persisted formats, protocol behavior, or backward compatibility;
- validation, authorization, security checks, recovery, retries, fallbacks, or error handling;
- an abstraction or extension point that appears deliberate or has multiple consumers;
- functionality covered by tests or documentation;
- code whose purpose or invariant is uncertain.

Apply approved and routine changes in the working tree. Leave everything else untouched.

## Constraints

- Preserve intended behavior unless the user explicitly approves a change.
- Do not remove code merely because it is unused until you have checked whether it is a public API, extension point, generated entry point, or intentionally retained compatibility surface.
- Do not replace clear code with a new abstraction solely to reduce line count.
- Do not weaken tests to permit simplification. Update tests only when an approved behavior change or a cleaner equivalent structure requires it.
- After edits, run the project's own checks through the same commands it documents or already uses: type check, lint, and the tests covering the affected code. If an approval is pending, leave the working tree in a consistent, passing state rather than a half-applied refactor.

## Completion

After editing, review the final diff for avoidable complexity, duplication, indirection, defensive branches, and accidental behavior changes. Report:

- what was simplified;
- which invariants the implementation now relies on;
- any significant removals you did not make because approval was not granted;
- the validation performed and its results.

Passing tests and type checks is necessary but not sufficient. Leave the affected code easier to understand and change.
