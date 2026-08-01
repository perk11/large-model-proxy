# Large Model Proxy

A Go-based reverse proxy that manages multiple resource-heavy AI models and
services on a single machine with limited VRAM/RAM. It automatically starts and
stops services on demand, evicting the least recently used running service when
resources are insufficient.

## Quick Start

```sh
# Build the proxy
make executable

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

| File                              | Purpose                                                                                     |
| --------------------------------- | ------------------------------------------------------------------------------------------- |
| `main.go`                         | Entry point, signal handling, and ResourceManager bookkeeping (connection counting, lookup) |
| `config.go`                       | Configuration loading, validation, and defaults (JSONC parsing)                             |
| `connection.go`                   | Per-service TCP listeners, client connection handling, and bidirectional traffic forwarding |
| `service.go`                      | Service lifecycle: on-demand start, health checks, and connecting to a running service      |
| `service_process.go`              | Service process spawning/stopping, output logging, and process-exit monitoring              |
| `resources.go`                    | Resource reservation, LRU eviction, and resource release logic                              |
| `openai_api.go`                   | Unified OpenAI API server and request routing to backends by model name                     |
| `management_api.go`               | Management HTTP server and embedded web dashboard assets                                    |
| `monitor_resources.go`            | Resource availability monitoring and change broadcasting                                    |
| `monitor_process_hook.go`         | Test-only synchronization hook for process-exit timing (compiled with the `testhooks` tag)  |
| `monitor_process_hook_default.go` | No-op production stub for `monitor_process_hook.go`                                         |
| `tty.go`                          | TTY/terminal handling utilities                                                             |

### Test Files

| File                        | Purpose                                             |
| --------------------------- | --------------------------------------------------- |
| `main_test.go`              | Core proxy integration tests                        |
| `config_test.go`            | Configuration parsing and validation tests          |
| `management_api_test.go`    | Management API endpoint tests                       |
| `monitor_resources_test.go` | Resource monitoring tests                           |
| `util_test.go`              | Shared test utilities and helpers                   |
| `test-server/main.go`       | Simulated backend service used in integration tests |

### Other Key Files

| File             | Purpose                                                                                       |
| ---------------- | --------------------------------------------------------------------------------------------- |
| `config.jsonc`   | Main configuration file (JSONC format). Never modify, you can create new config if necessary. |
| `Makefile`       | Build and test automation                                                                     |
| `management-ui/` | Web dashboard frontend source                                                                 |
| `test-configs/`  | Test configuration fixtures                                                                   |

### Running Tests

```sh
# Full test suite
make test
```

> **Note**: Tests use `test-server/test-server` as a simulated backend. Both
> binaries must be built before running tests. `make test` handles this
> automatically.

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

## Agent rules

Due to its concurrent nature, race conditions are very common in
large-model-proxy, making manual testing impractical. Therefore, close to 100%
automated test coverage is required.

- Add tests for new features.
- Follow TDD, write tests before implementing features.
- Prefer meaningful and descriptive variable names, avoid abbreviations.
- Keep comments minimal and to the point. Focus on describing why something is happening, not what is happening.
- Run `make test` before submitting.
