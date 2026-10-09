---
description: Run gomutants on changed code and propose tests for surviving mutants
argument-hint: [packages... | --since <ref>]
allowed-tools: Bash(gomutants *), Bash(go run *), Bash(git *), Bash(jq *), Bash(cat *), Bash(which *), Read, Glob
---

You are running `gomutants` (Go mutation testing) and using the report to find test gaps in the user's project.

## Step 0 — locate gomutants

Check whether `gomutants` is on PATH (`which gomutants`).

- If found, use `gomutants` directly.
- If missing, fall back to `go run github.com/szhekpisov/gomutants@latest`. Tell the user once that you are using the fallback and that they can install the binary with:
  - `go install github.com/szhekpisov/gomutants@latest`, or
  - downloading a release from https://github.com/gomutants/gomutants/releases.

In the rest of these instructions, `<gomutants>` means whichever of the two you picked.

## Step 1 — pick a scope

Parse `$ARGUMENTS`:

- If it contains one or more package patterns (e.g. `./internal/foo`, `./...`), use them as positional args.
- If it contains `--since <ref>` (e.g. `--since main`, `--since HEAD~1`), pass `-changed-since <ref>` and default packages to `./...`.
- If empty, default to `-changed-since main ./...`. If the repo has no `main` branch, fall back to `./...` and tell me.

Run from the repo root (the directory containing `go.mod`). If the user invoked you from a subdirectory of a Go module, walk up to the module root first.

## Step 2 — run gomutants

```
<gomutants> -quiet \
  -output /tmp/gomutants-report.json \
  -html-output /tmp/gomutants-report.html \
  [scope from step 1]
```

Notes:
- `-quiet` suppresses progress output; the JSON file has everything needed for analysis.
- `-html-output` writes a self-contained, click-through HTML viewer (per-file efficacy sidebar, annotated source). Surface its path in step 5 so the user can open it.
- Do **not** pass `-dry-run` — real KILLED/LIVED status is required.
- Do **not** pass `-cache=off`. The default `.gomutants-cache.json` is on, which makes repeat runs in the same session fast.
- Exit codes 10 / 11 mean the efficacy / coverage thresholds were not met. Both reports still wrote, so continue.
- Exit code 2 means the invocation or configuration is invalid. Stop and surface the error; do not continue with a stale or missing report.
- `INFRA ERROR` entries mean the host ran out of a recognized resource or hit an I/O failure. Do not treat them as killed mutants or propose tests for them; report the count and recommend rerunning after the environment is healthy.
- If the run is taking visibly long on `./...`, narrow to the package with the most changed files and tell the user you did so.

## Step 3 — extract surviving mutants

Read `/tmp/gomutants-report.json`. Schema:

```
{
  "files": [
    {
      "file_name": "...",
      "mutations": [
        {
          "id": "pkg/file.go:FuncName:TYPE#1",
          "type": "...",
          "status": "LIVED|KILLED|NOT COVERED|NOT VIABLE|TIMED OUT|INFRA ERROR",
          "line": N,
          "column": N,
          "original": "...",
          "replacement": "..."
        }
      ]
    }
  ]
}
```

`id` is a stable per-mutant handle, formatted `file:function:TYPE#n`, where the file part is relative to the module root. It is anchored to the enclosing function rather than to a line, and does not depend on which packages the run was scoped to, so it stays the same across runs even when the file is reformatted, edited elsewhere, or covered by a different package argument — carry it into your output so the user can match a suggestion to the same mutant on a later run.

Filter to `status == "LIVED"`. Note the `NOT COVERED` count per file separately as a secondary signal — those mutants no test even exercises. Count `INFRA ERROR` entries separately as an incomplete environmental outcome; they are intentionally excluded from test proposals and the incremental cache.

## Step 4 — propose tests

For up to ~10 surviving mutants (prioritise files with the most survivors):

1. Read the source file around `line` to understand what the mutation changes. The `original` → `replacement` diff is the key (e.g. removing a `defer`, flipping `<` to `<=`, dropping a statement).
2. Use `Glob` to find the corresponding `*_test.go` and skim existing test names so suggestions don't collide.
3. Output one block per mutant:

   ```
   ### <file>:<line>  —  <type>   (status: LIVED)
   `<id>`
   `<original>`  →  `<replacement>`

   **Why it survived:** <one sentence — what existing tests fail to assert>

   **Kill it:**
   ```go
   func TestXxx_<short_name>(t *testing.T) {
       // ...
   }
   ```
   ```

4. If the user accepts a suggestion and adds the test, you can verify it actually kills that mutant without re-testing every other one. Setup is not skipped — coverage collection and the baseline measurement still run the suite — but only the one named mutant is tested after that:

   ```
   <gomutants> -quiet \
     -output /tmp/gomutants-one-mutant.json \
     --run-mutant-id '<id>' --threshold-efficacy 100 <same package arg>
   ```

   Exit 0 means killed, 10 means it still survives, and any other code means the run produced no verdict — read the error before reporting anything. Report which one you got rather than assuming the test worked. Keep `-output` pointed at `/tmp`: without it the run writes `mutation-report.json` into the user's repo (or overwrites the path their config sets).

## Step 5 — wrap up

End with a two-line summary:

```
N surviving mutants across M files; proposed K new tests.
HTML report: /tmp/gomutants-report.html  (open with `open /tmp/gomutants-report.html` on macOS, or `xdg-open` on Linux)
```

If the report contains infrastructure errors, state `<count> INFRA ERROR outcomes need a rerun after the host issue is fixed.` immediately before that two-line summary.

Do **not** edit any files — proposals only. If the user wants them applied, they will ask.
