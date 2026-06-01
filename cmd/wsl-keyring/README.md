# wsl-keyring

`wsl-keyring` is a Secret Service provider daemon for Linux/WSL. It implements the D-Bus Secret Service specification (`org.freedesktop.secrets`), allowing standard Linux desktop applications (like VS Code, Git credential helper, or Chrome) to store and retrieve credentials securely using **1Password** (`op.exe`) as the persistent storage backend.

## Features
- Implements the standard `org.freedesktop.secrets` D-Bus API.
- Stores secrets in 1Password using the 1Password CLI (`op.exe`).
- Supports replaceable storage backends (includes an in-memory backend for testing).
- Designed for WSL (Windows Subsystem for Linux), defaulting to run the Windows version of the 1Password CLI (`op.exe`) to share biometric authentication.

---

## Prerequisites
1. **1Password CLI (`op.exe`)**:
   Ensure that the Windows version of the 1Password CLI (`op.exe`) is installed and configured on your Windows host, and its path is visible inside your WSL environment.
2. **D-Bus**:
   A running D-Bus session bus is required inside WSL.

---

The tool is configured using environment variables:

| Env | Description | Default |
|-----|-------------|---------|
| `OP_VAULT` | The name of the 1Password vault where secrets are stored. | `wsl-keyring` |
| `USE_IN_MEMORY` | Uses an ephemeral in-memory storage instead of 1Password. Useful for testing. | `false` |
| `OP_BINARY` | The binary command to call the 1Password CLI. | `op.exe` |
| `OP_AUTH_CACHE_TTL` | Duration to reuse a successful `op whoami` authentication check before another vault operation triggers a fresh check. Set to `0s` to disable reuse while keeping single-flight coalescing. | `30s` |
| `WSL_KEYRING_SECRET_CACHE_TTL` | Sliding TTL for decrypted secrets cached in memory after a read or write. | `60s` |
| `WSL_KEYRING_AUTH_CHECK_MIN_SPACING` | Minimum interval between background `op whoami` checks after a secret-cache hit. | `5s` |
| `WSL_KEYRING_AUTH_CHECK_TIMEOUT` | Timeout for the background `op whoami` check. | `2s` |

---

## Running the Daemon

To start the daemon, simply run:
```bash
# Build the binary
go build -o dist/wsl-keyring ./cmd/wsl-keyring

# Start the daemon
./dist/wsl-keyring
```

## Setup D-Bus Activation (Optional)

You can automatically generate the D-Bus service file so that the keyring daemon starts automatically whenever a client requests a secret. Run the built binary with the `INIT=true` environment variable:

```bash
INIT=true ./dist/wsl-keyring
```

This will automatically find the absolute path of the executable and write the definition file to:
`~/.local/share/dbus-1/services/org.freedesktop.secrets.service`

---

To run with a custom vault:
```bash
OP_VAULT="My Keyring Vault" ./dist/wsl-keyring
```

---

## Testing / Local Verification

You can test the daemon without 1Password by using the in-memory storage:
```bash
# Run the daemon with in-memory storage
USE_IN_MEMORY=true ./dist/wsl-keyring
```

Once running, you can use standard Linux tools to test:
```bash
# Store a secret
secret-tool store --label="Test Secret" application my-app

# Lookup a secret
secret-tool lookup application my-app
```
