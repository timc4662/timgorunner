# timgorunner

Runs `go test -json`, shows each test live on the console, prints an overall summary,
and writes a self-contained HTML report (filter by status, collapsible output, dark mode).

    go build -o timgorunner .
    ./timgorunner ./... --output=results.html
    ./timgorunner ./internal/... -race -count=1 -run TestFoo --output=out/report.html

Exit code matches `go test` (non-zero on failure). Use `--no-color` or `NO_COLOR=1` to disable colours;
`--` passes any remaining flags straight to `go test`.
