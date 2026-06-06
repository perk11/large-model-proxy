# Large Model Proxy

A Go-based reverse proxy that manages multiple resource-heavy AI models and
services on a single machine with limited VRAM/RAM. It automatically starts and
stops services on demand, evicting the least recently used running service when
resources are insufficient.

## Quick Start

```sh
# Build the proxy
make executable

# Build the test helper server (required for tests)
make build-test-server

# Run tests
make test

# Run the proxy with a config file
./large-model-proxy -c path/to/config.jsonc
```

## Key Concepts

- **Service Lifecycle Management**: Services are started on-demand when a client
  connects and automatically stopped after a configurable idle timeout or when
  resources are needed elsewhere.
- **Resource-Aware Scheduling**: Before starting a service, the proxy checks if
  required resources (VRAM, RAM, etc.) are available. If not, it evicts the
  least recently used (LRU) running service to free resources.
- **Transparent Proxying**: Clients connect to dedicated ports per service or a
  unified OpenAI API port. The proxy forwards traffic to the actual service
  process, handling startup delays transparently.
- **OpenAI API Compatibility**: Exposes `/v1/completions`,
  `/v1/chat/completions`, `/v1/models`, and `/v1/models/{model}` endpoints,
  routing requests to the appropriate backend model based on the `model` field.
- **Management API & Dashboard**: A built-in HTTP API (`/status`) and web UI
  provide real-time visibility into service status, resource usage, and active
  connections.

## Architecture

```
Client → large-model-proxy → [Service Process]
                          ↘ Resource Monitor
                          ↘ Management API
```

### Request Flow

1. Client connects to a service port or sends an OpenAI API request.
2. Proxy checks if the target service is already running.
3. If not running, proxy verifies required resources are available.
4. If resources are insufficient, proxy waits for resources to become available.
5. Once resources are available, proxy starts the service process and waits for
   it to become available.
6. Proxy waits for the healthcheck to pass. If no healthcheck is defined,
   connecting to the service is used as a healthcheck.
7. Proxy forwards traffic between client and service.
8. After the configured idle timeout (if defined), proxy stops the service to
   free resources.

### Source Files

| File                   | Purpose                                                                           |
| ---------------------- | --------------------------------------------------------------------------------- |
| `main.go`              | Core proxy logic, HTTP handlers, service lifecycle orchestration, signal handling |
| `config.go`            | Configuration loading, validation, and defaults (JSONC parsing)                   |
| `management_api.go`    | Management HTTP server and embedded web dashboard assets                          |
| `monitor_resources.go` | Resource checking                                                                 |
| `tty.go`               | TTY/terminal handling utilities                                                   |

### Test Files

| File                     | Purpose                                             |
| ------------------------ | --------------------------------------------------- |
| `main_test.go`           | Core proxy integration tests                        |
| `config_test.go`         | Configuration parsing and validation tests          |
| `management_api_test.go` | Management API endpoint tests                       |
| `monitor_resources.go`   | Resource monitoring tests                           |
| `util_test.go`           | Shared test utilities and helpers                   |
| `test-server/main.go`    | Simulated backend service used in integration tests |

### Other Key Files

| File             | Purpose                                                                                       |
| ---------------- | --------------------------------------------------------------------------------------------- |
| `config.jsonc`   | Main configuration file (JSONC format). Never modify, you can create new config if necessary. |
| `Makefile`       | Build and test automation                                                                     |
| `management-ui/` | Web dashboard frontend source                                                                 |
| `test-configs/`  | Test configuration fixtures                                                                   |

## Makefile Targets

| Target                   | Description                                            |
| ------------------------ | ------------------------------------------------------ |
| `make all`               | Build executable and test-server (default)             |
| `make executable`        | Build `large-model-proxy` binary                       |
| `make test`              | Build everything and run tests with `-v -parallel 500` |
| `make clean`             | Remove built binaries and test artifacts               |
| `make build-test-server` | Build the test helper server binary                    |

## Configuration

Configuration uses JSONC (JSON with comments and trailing commas). Each service
defines:

- `Command` / `Args`: Process to start
- `ListenPort`: External port clients connect to (optional)
- `ProxyTargetHost` / `ProxyTargetPort`: Internal port the service binds to
- `ResourceRequirements`: VRAM, RAM, or any other custom resources needed
- `HealthcheckCommand` / `HealthcheckIntervalMilliseconds`: Readiness probe
- `ShutDownAfterInactivitySeconds`: Idle timeout before stopping
- `StartupTimeoutMilliseconds`: Max time to wait for service startup
- `RestartOnConnectionFailure`: Whether to restart on connection errors
- `ConsiderStoppedOnProcessExit`: Track services that detach from the proxy
- `KillCommand`: Custom termination command (e.g., `docker kill <name>`)
- `LogFilePath`: Override default `logs/{name}.log`
- `OpenAiApi`: Enable OpenAI API routing for this service
- `ServiceUrl`: URL rendered in the dashboard (supports `{{.PORT}}` template)

### Resource Checking

Resources can be specified as static integers:

```jsonc
"ResourcesAvailable": {
  "VRAM-GPU-0": 24000,
  "RAM": 32000
}
```

Or backed by `CheckCommand` shell commands for dynamic checking:

```jsonc
"ResourcesAvailable": {
  "VRAM-GPU-0": {
    "Amount": 24000,
    "CheckCommand": "nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits -i 0",
    "CheckIntervalMilliseconds": 1000
  }
}
```

### Service Identification

- Services are identified by `Name` and mapped to `ListenPort`.
- `ProxyTargetPort` is the actual port the service binds to internally;
  `ListenPort` is the external port clients use.
- If `ListenPort` is omitted, the service is only available via the OpenAI API.
- `ServiceUrl` is rendered in the management dashboard; if omitted,
  `DefaultServiceUrl` template is used.

### Running Tests

```sh
# Full test suite
make test
```

> **Note**: Tests use `test-server/test-server` as a simulated backend. Both
> binaries must be built before running tests. `make test` handles this
> automatically.

### Code Style

- Follow standard Go conventions (`gofmt`).
- Use meaningful variable names; avoid abbreviations.

### OpenAI API-only Services

Omit `ListenPort` to make a service accessible only via the unified OpenAI API:

```jsonc
{
  "Name": "Qwen2.5-7B",
  "OpenAiApi": true,
  "ProxyTargetHost": "localhost",
  "ProxyTargetPort": 18082,
  "Command": "vllm",
  "Args": "serve Qwen/Qwen2.5-7B-Instruct --port 18082",
}
```

## Contributing

Due to its concurrent nature, race conditions are very common in
large-model-proxy, making manual testing impractical. Therefore, close to 100%
automated test coverage is required.

- Add tests for new features.
- Run `make test` before submitting.
