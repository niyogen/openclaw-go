# Branch protection setup for `main`

Go to **Settings → Branches → Branch protection rules → Add rule** on your
GitHub repository, target the `main` (or `master`) branch, and enable:

## Required status checks

Add the following check as **required**:

```
pr-checks / all-checks-passed
```

This single check collects all gates:

| Job | What it does |
|-----|--------------|
| `pr-checks / unit-tests (ubuntu-latest)` | `go test -race ./...` on Ubuntu |
| `pr-checks / unit-tests (macos-latest)` | `go test -race ./...` on macOS |
| `pr-checks / unit-tests (windows-latest)` | `go test ./...` on Windows |
| `pr-checks / e2e-go (ubuntu-latest)` | In-process Go E2E suite |
| `pr-checks / e2e-go (macos-latest)` | In-process Go E2E suite |
| `pr-checks / e2e-go (windows-latest)` | In-process Go E2E suite |
| `pr-checks / e2e-shell` | Shell smoke test (Linux binary) |
| `pr-checks / e2e-ps` | PowerShell smoke test (Windows binary) |
| `pr-checks / docker-build` | Dockerfile build lint (no push) |
| `pr-checks / lint` | golangci-lint |

## Recommended settings

- ✅ Require a pull request before merging
- ✅ Require status checks to pass before merging
  - ✅ Require branches to be up to date before merging
  - **Required check**: `pr-checks / all-checks-passed`
- ✅ Require conversation resolution before merging
- ✅ Restrict who can push to matching branches (optional)
- ✅ Do not allow bypassing the above settings

## Workflow trigger summary

| Event | Workflows that fire |
|-------|---------------------|
| PR opened / updated → `main` | `pr-checks` (all gates), `CI`, `E2E`, `Docker` (build-only) |
| Push to `main` | `CI`, `E2E`, `Docker` (build + push), `pr-checks` skipped |
| Push tag `v*` | `Release` (cross-compile + GitHub Release) |
