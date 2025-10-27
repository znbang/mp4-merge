# Repository Guidelines

Contributors extend a Go-based MP4 merging library and CLI, so updates should keep the codebase tidy, reproducible, and portable.

## Project Structure & Module Organization
The root Go module `mp4merge` exposes the core merge helpers in files such as `join.go`, `writer.go`, and `types.go`, with supporting logic split into focused files like `insta360.go` and `progress.go`. CLI tooling lives in `cmd/mp4merge/main.go`, mirroring the library API for end users. Tests currently reside in `merge_test.go`; keep additional suite files alongside the code they cover, and document exported behavior in `doc.go` for pkg.go.dev.

## Build, Test, and Development Commands
Use `go build -o mp4merge ./cmd/mp4merge` to compile the CLI locally, mirroring the shipped binary. Run `go test ./...` before every push to exercise library paths and verify platform-specific shims in `update_time*.go`. During development, `go run ./cmd/mp4merge --out sample.mp4 clip1.mp4 clip2.mp4` is a quick smoke test for argument parsing and progress callbacks.

## Coding Style & Naming Conventions
All Go files should be formatted with `gofmt` (tabs for indentation, newline at EOF); `goimports` maintains import groups. Exported symbols use PascalCase so documentation renders correctly, while internal helpers stay lowerCamelCase. Prefer small, purpose-built files and keep filename suffixes descriptive (`*_windows.go`, `_test.go`) to trigger the correct build tags automatically.

## Testing Guidelines
Write table-driven unit tests under `*_test.go` files, naming functions `TestComponentScenario` for clarity. When adding media fixtures, place them under a short-lived temp directory in the test rather than committing binaries. Aim for coverage on edge cases like uneven atom sizes or missing metadata, and include regression tests for previously reported Insta360 quirks.

## Commit & Pull Request Guidelines
Commit messages should be imperative (`Fix progress ticker`) and mention the touched area when useful (`cli: ...`, `library: ...`). Open PRs with a brief summary, validation commands (e.g., `go test ./...`), and link any issue or customer report. If behavior changes, attach a before/after description or CLI transcript so reviewers can verify the new flow quickly.

## Platform & Media Notes
Keep cross-platform abstractions intact: Windows timestamp handling belongs in `update_time_windows.go`, while Unix-friendly logic stays in `update_time.go`. Large media files should never enter the repo; document sample sources in the PR instead.
