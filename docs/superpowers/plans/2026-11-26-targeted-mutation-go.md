# Targeted Mutation Testing for Go implementation plan

**Goal** — Ship a Go CLI that reports actionable mutation findings for local and CI workflows.

1. Implement a typed rank representation for native integer values. Derive candidates from the nearest observations and verify each case against an exhaustive oracle and the Lean specification.
2. Implement package discovery, scope, runtime source rewriting, atomic observation transport, and isolated overlays. Exercise real packages and failed builds via the campaign API.
3. Build independent test binaries and replay selected package processes. Require clean baselines and activation evidence before assigning a verdict. Verify the state machine with TLC.
4. Expose stable text and JSON reports, scoped runs and CI exits. Test the installed binary against a multi-package module and compare equivalent real-tool behavior before making performance claims.
