# gomutants — Claude Code plugin

Run [gomutants](https://github.com/gomutants/gomutants) (Go mutation testing) from Claude Code and turn surviving mutants into concrete `*_test.go` cases.

The plugin exposes one slash command, `/gomutants:mutants`. It runs `gomutants` on changed code, parses the JSON report, and proposes new tests that would kill each surviving mutant — without editing any files. It also writes a self-contained interactive HTML report to `/tmp/gomutants-report.html` for click-through inspection.

## Install

```
/plugin marketplace add gomutants/gomutants
/plugin install gomutants@gomutants
```

Once approved for the official directory, the install becomes:

```
/plugin install gomutants@claude-plugins-official
```

## Use

```
/gomutants:mutants                    # default: --changed-since main ./...
/gomutants:mutants ./internal/foo     # scope to a package
/gomutants:mutants --since HEAD~1     # scope by git ref
```

Output, per surviving mutant:

```
### internal/foo/bar.go:42  —  CONDITIONALS_BOUNDARY   (status: LIVED)
`<` → `<=`

**Why it survived:** existing tests only cover the inequality case; no test asserts the boundary value.

**Kill it:**
```go
func TestBar_BoundaryEquality(t *testing.T) {
    // ...
}
```

The wrap-up line points at the HTML report: `open /tmp/gomutants-report.html` (macOS) or `xdg-open` (Linux).

## Requirements

- Go 1.26+ on `PATH` (the plugin shells out to `go test` on the project under test).
- `gomutants` on `PATH` is preferred (`go install github.com/gomutants/gomutants@latest`). If absent, the plugin falls back to `go run github.com/gomutants/gomutants@latest` — works out of the box, slower on first run.
- A git repository for `--changed-since`-style invocations.

## What it doesn't do

- It never edits source files. Test proposals are printed for the user to apply.
- It doesn't alter `gomutants` defaults beyond `-quiet`, `-output`, and `-html-output`. The incremental cache (`.gomutants-cache.json`, on by default) is left enabled, so repeat runs in a session are fast.
- It doesn't bundle a `gomutants` binary; it expects the Go toolchain to be available.

## Links

- [gomutants](https://github.com/gomutants/gomutants) — the underlying tool, full CLI reference, mutators, and CI integration
- [Plugin source](https://github.com/gomutants/gomutants/tree/main/plugin) — manifest and command body
- [Marketplace manifest](https://github.com/gomutants/gomutants/blob/main/.claude-plugin/marketplace.json)

## License

[MIT](https://github.com/gomutants/gomutants/blob/main/LICENSE) — same as the parent project.
