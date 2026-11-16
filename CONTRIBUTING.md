# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Before you open a PR

```sh
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
```

CI runs the same checks plus a [gitleaks](https://github.com/gitleaks/gitleaks) secret scan.

## Ground rules

- Tests use synthetic keys and local HTTP stubs. Never call the live pey API from a test, and never commit a real key, journal or `.env` file.
- Money stays in integer centavos, always under the configured cap.
- Keep the safety guards (fixed host, cap, journal, redaction, DIRECT/STORE exclusivity). If a change weakens one, explain why in the PR.
- One focused change per PR. Commit messages follow `<type>: <description>` (`feat`, `fix`, `docs`, `test`, `refactor`, `chore`).

By contributing you agree that your work is released under the [MIT License](LICENSE).
