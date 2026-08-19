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

## Validate changes

Once source code exists, format, analyze, test, and build the project:

```sh
gofmt -w .
go vet ./...
go test ./...
go build ./...
```
