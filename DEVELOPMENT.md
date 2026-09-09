# Development

## Set up Go on macOS

1. Install Go using Homebrew:

   ```sh
   brew install go
   ```

   Alternatively, download and run the installer from the [official Go website](https://go.dev/doc/install).

2. Restart the terminal and verify the installation:

   ```sh
   go version
   ```

3. Change to the root of the cloned repository:

   ```sh
   cd /path/to/cli
   ```

4. Download the dependencies declared in `go.mod` and verify them against `go.sum`:

   ```sh
   go mod download
   go mod verify
   ```

5. Install the Go language server:

   ```sh
   go install golang.org/x/tools/gopls@latest
   ```

6. Add installed Go tools to `PATH` in `~/.zshrc`:

   ```sh
   export PATH="$PATH:$(go env GOPATH)/bin"
   ```

   Reload the shell configuration:

   ```sh
   source ~/.zshrc
   ```

7. If using Visual Studio Code, install the official **Go** extension by Google. It integrates formatting, tests, debugging, and `gopls`.

## Run during development

Run the CLI directly from the current source code:

```sh
go run ./cmd/awdev
```

`go run` compiles the current source into a temporary executable and runs it immediately. There is no need to build or install `awdev` after every change.

Pass commands and arguments in the same way as with the installed executable:

```sh
go run ./cmd/awdev --help
go run ./cmd/awdev init
go run ./cmd/awdev run github 123
go run ./cmd/awdev status github 123 --json
go run ./cmd/awdev resume github 123
go run ./cmd/awdev retry github 123
```

End-to-end development requires a non-shallow Git checkout with an `origin`
matching the repository reported by an authenticated GitHub CLI, plus an
authenticated CLI for the configured agent (Codex or Claude Code). The commands
run in the foreground; cancellation
terminates the active child process tree and leaves durable state for
inspection. See [README.md](README.md) for configuration and lifecycle details
and [docs/manual-recovery.md](docs/manual-recovery.md) before altering failed
workflow state.

The usual local development cycle is:

```text
edit source -> go run ./cmd/awdev
```

## Build and run

Build the executable inside the ignored `bin` directory and run it:

```sh
go build -o bin/awdev ./cmd/awdev
./bin/awdev
```

To install the executable into the Go binary directory and invoke it as `awdev` from any directory:

```sh
go install ./cmd/awdev
awdev
```

## Validate changes

Format, analyze, test, and build the project:

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
go build ./...
```

The `internal/e2e` package builds the real executable, uses real temporary Git
repositories with local bare origins, and substitutes deterministic `gh`,
`codex`, and `claude` executables. It does not require network access or live
credentials.
