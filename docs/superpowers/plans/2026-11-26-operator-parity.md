# Operator parity implementation plan

1. Add typed source edit discovery and classify candidates from declarations, conditions, statements, and returns. Use PICT and explicit peer transformations to verify scoping and operator precedence.
2. Materialize each edit inside its worker. Validate compiled results and bound retries for imports and locals made unused by a mutation. Verify the source replay state model with TLC.
3. Exercise the existing CLI entry point and measure deterministic mutation workloads. Document the supported variants, and review the catalogue and replay changes with independent Astra reviews.
