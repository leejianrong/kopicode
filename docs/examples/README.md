# Configuration examples

Example `.kopicode/config.toml` files for different project types.

## Use one

```bash
cp docs/examples/go-project.toml .kopicode/config.toml
```

Edit the `model` field if you want a model other than the default, then run
`kopicode` in your project directory.

| File | Project type |
| --- | --- |
| `go-project.toml` | Go |
| `node-project.toml` | JavaScript/TypeScript |
| `python-project.toml` | Python |
| `multi-language.toml` | Multiple languages in one repo |

## What the file can hold

kopicode's reader is intentionally not a full TOML implementation — flat
top-level `key = "value"` lines and comments only, per
[ADR-0007](../adr/0007-model-selection-and-harness-config-shape.md) decision 2.
The keys it reads:

- `model` — a registered model id (see the main [README](../../README.md#3-pick-a-model)
  for the current list).
- `harness` — a registered harness config name, when overriding the one the
  model resolves to by default.
- `verify` — an argv array, e.g. `verify = ["go", "test", "./..."]`, that pins
  the forced-verification command instead of relying on discovery. It must be
  an array, never a shell string: kopicode records and replays argv, and there
  is no shell in the loop to re-split one.

Precedence, highest first: `--model`/`--harness` on the command line, this
file, then the built-in default.

## See also

- [Main README — Quickstart](../../README.md#quickstart)
- [Architecture Decision Records](../adr/)
- [PRD](../PRD.md)
