---
name: go-table-test
description: Add or extend a table-driven test in this Go project. Use when asked to add or improve test coverage for a function.
---

# Writing a table-driven test

This project tests with table-driven subtests and no assertion library. When you
add coverage for a function, follow the shape the existing tests already use:

1. Find the function's `_test.go` file, or create `foo_test.go` next to `foo.go`.
2. Write one test function per behaviour. Inside it, declare a slice of case
   structs — each with a `name`, the inputs, and the `want`.
3. Run each case as its own subtest: `t.Run(tc.name, func(t *testing.T) { ... })`.
4. Assert with a plain comparison and a `t.Errorf` that prints call, got, and
   want — `t.Errorf("Foo(%v) = %v, want %v", tc.in, got, tc.want)`. Do not add a
   third-party assertion dependency.
5. Confirm it passes: `go test ./path/to/pkg`.

Name each case for the behaviour it pins ("negative input rounds toward zero"),
not for the literal input. Keep cases minimal — one behaviour each.
