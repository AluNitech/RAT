# ModernRat Go Clients

This directory contains executable clients for interacting with the ModernRat gRPC services.

## Binaries

- `cmd/main.go`: endpoint-side agent that registers a user and serves remote shell commands.
- `cmd/adminshell/main.go`: administrator console for opening a remote shell against a registered user.

## Building

```bash
cd go-client
go build -o ../bin/client ./cmd        # endpoint agent
go build -o ../bin/adminshell ./cmd/adminshell
```

## Running The Admin Shell CLI

```bash
# Environment variables (adjust as needed)
SERVER_ADDR="localhost:50051"
ADMIN_PASSWORD="your-admin-password"
TARGET_USER_ID="user_id_from_list"

# Build binary first (see above) or run via go run
cd go-client
JWT_SECRET="$(openssl rand -hex 32)" # only needed when starting server separately

# Invoke admin shell (use --help for options)
go run ./cmd/adminshell \
  --addr "$SERVER_ADDR" \
  --user "$TARGET_USER_ID" \
  --password "$ADMIN_PASSWORD"
```

The CLI will request an admin token via `GenerateAdminToken`, start the `AdminShell` gRPC stream, and bridge your terminal input/output with the remote endpoint. Press `Ctrl+C` to terminate the session.
