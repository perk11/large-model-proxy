# Large Model Proxy

Large Model Proxy is designed to make it easy to run multiple resource-heavy Large Models (LM) on the same machine with a limited amount of VRAM/other resources.
It listens on a dedicated port for each proxied LM and/or a single port for OpenAI API, making LMs always available to the clients connecting to these ports.

## How it works

Upon receiving a connection, if the LM on this port or `model` specified in JSON payload to an OpenAI API endpoint is not already started, it will:

1. Verify if the required resources are available to start the corresponding LM.
2. If resources are not available, it will automatically stop the least recently used LM to free up contested resources.
3. Start the LM.
4. Wait for the LM to be available on the specified port.
5. Wait for healthcheck to pass.

Then, it will proxy the connection from the LM to the client that connected to it.
To the client, this should be fully transparent, with the only exception being that receiving any data on the connection takes longer if the LM had to be spun up.

## Installation

**Ubuntu and Debian**: Download the deb file attached to the [latest release](https://github.com/perk11/large-model-proxy/releases/latest).

**Arch Linux**: Install from [AUR](https://aur.archlinux.org/packages/large-model-proxy).

**Other Distros**:

1. Install go

2. Clone the repository:
   ```sh
   git clone https://github.com/perk11/large-model-proxy.git
   ```
3. Navigate into the project directory:
   ```sh
   cd large-model-proxy
   ```
4. Build the project:
   ```sh
   go build -o large-model-proxy
   ```
   or
   ```sh
   make
   ```

**Windows**: Not currently tested, but should work in WSL using "Ubuntu" or "Other Distros" instruction.
It will probably not work on Windows natively, as it is using Unix Process Groups.

**macOS**: Might work using "Other Distros" instruction, but I do not own any Apple devices to check, please let me know if it does!

## Configuration

Configuration support JSONC (JSON with comments and trailing commas).

Below is an example `config.jsonc`:

```jsonc
{
  "DefaultServiceUrl": "http://localhost:{{.PORT}}/",
  "OpenAiApi": {
    "ListenPort": "7070",
  },
  "ManagementApi": {
    "ListenPort": "7071",
  },
  "MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds": 1200,
  "ShutDownAfterInactivitySeconds": 120,
  "ResourcesAvailable": {
    "VRAM-GPU-1": {
        "Amount": 24000,
        "CheckCommand": "nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i 0",
        "CheckIntervalMilliseconds": 1000,
    }
    "RAM": 32000,
  },
  "Services": [
    {
      "Name": "automatic1111",
      "ListenPort": "7860",
      "ProxyTargetHost": "localhost",
      "ProxyTargetPort": "17860",
      "Command": "/opt/stable-diffusion-webui/webui.sh",
      "Args": "--port 17860",
      "WorkDir": "/opt/stable-diffusion-webui",
      "ShutDownAfterInactivitySeconds": 600,
      "RestartOnConnectionFailure": true,
      "ResourceRequirements": {
        "VRAM-GPU-1": 2000,
        "RAM": 30000,
      },
    },
    {
      "Name": "Gemma-27B",
      "OpenAiApi": true,
      "ListenPort": "8081",
      "ProxyTargetHost": "localhost",
      "ProxyTargetPort": "18081",
      "Command": "/opt/llama.cpp/llama-server",
      "Args": "-m /opt/Gemma-27B-v1_Q4km.gguf -c 8192 -ngl 100 -t 4 --port 18081",
      "ServiceUrl": "http://gemma-proxy-server/",
      "HealthcheckCommand": "curl --fail http://localhost:18081/health",
      "HealthcheckIntervalMilliseconds": 200,
      "RestartOnConnectionFailure": false,
      "ResourceRequirements": {
        "VRAM-GPU-1": 20000,
        "RAM": 3000,
      },
    },
    {
      "Name": "Qwen/Qwen2.5-7B-Instruct",
      "OpenAiApi": true,
      "ProxyTargetHost": "localhost",
      "ProxyTargetPort": "18082",
      "Command": "/home/user/.conda/envs/vllm/bin/vllm",
      "LogFilePath": "/var/log/Qwen2.5-7B.log",
      "Args": "serve Qwen/Qwen2.5-7B-Instruct --port 18082",
      "ConsiderStoppedOnProcessExit": true,
      "ServiceUrl": null,
      "ResourceRequirements": {
        "VRAM-GPU-1": 17916,
      },
    },
    {
      "Name": "ComfyUI",
      "ListenPort": "8188",
      "TargetPort": "18188",
      "Command": "docker",
      "Args": "run --rm --name comfyui --device nvidia.com/gpu=all -v /opt/comfyui:/workspace -p 18188:8188 pytorch/pytorch:2.6.0-cuda12.6-cudnn9-devel /bin/bash -c 'cd /workspace && source .venv/bin/activate && apt update && apt install -y git && pip install -r requirements.txt && python main.py --listen'",
      "KillCommand": "docker kill comfyui",
      "StartupTimeoutMilliseconds": 60000,
      "RestartOnConnectionFailure": true,
      "ShutDownAfterInactivitySeconds": 600,
      "ResourceRequirements": {
        "VRAM-GPU-1": 20000,
        "RAM": 16000,
      },
    },
  ],
}
```

Below is a breakdown of what this configuration does:

1. Any client can access the following services:
   - Automatic1111's Stable Diffusion web UI on port 7860
   - llama.cpp with Gemma2 on port 8081
   - OpenAI API on port 7070, supporting Gemma2 via llama.cpp and Qwen2.5-7B-Instruct via vLLM, depending on the `model` specified in the JSON payload.
   - Management server on port 7071 (optional). If `ManagementServer.ListenPort` is not specified in the config, the management server will not run.
   - ComfyUI on port 8188 through a Docker container, which exposes the internal port 8188 as 18188 on the host, which is then proxied back to 8188 when active.
2. Internally, large-model-proxy will expect Automatic1111 to be available on port 17860, Gemma27B on port 18081, Qwen2.5-7B-Instruct on port 18082, and ComfyUI on port 18188 once it runs the commands given in the "Command" parameter and healthcheck passes.
3. This config allocates up to 24GB of VRAM and 32GB of RAM for them. No more GPU or RAM will be attempted to be used (assuming the values in ResourceRequirements are correct).
4. The Stable Diffusion web UI is expected to use up to 3GB of VRAM and 30GB of RAM, while Gemma27B will use up to 20GB of VRAM and 3GB of RAM, Qwen2.5-7B-Instruct up to 18GB of VRAM and no RAM (for example's sake), and ComfyUI up to 20GB of VRAM and 16GB of RAM.
5. Automatic1111, Gemma2, and ComfyUI logs will be in the `logs/` directory of the current dir, while Qwen logs will be in `/var/log/Qwen2.5-7B.log`.
6. When ComfyUI is no longer in use, its container will be killed using the `docker kill comfyui` command. Other services will be terminated normally.
7. `StartupTimeoutMilliseconds` in starting ComfyUI makes large-model-proxy wait up to 60 seconds before giving up and considering the ComfyUI startup failed (as opposed to the default value of 10 minutes).
8. Service URLs are configured as follows:
   - **Automatic1111**: Uses the default URL template (`DefaultServiceUrl`) which resolves to `http://localhost:7860/`
   - **Gemma27B**: Uses a custom static URL `http://gemma-proxy-server/` (no port templating)
   - **Qwen2.5-7B-Instruct**: Explicitly set to `null`, so no URL will be generated even though a default is available
   - **ComfyUI**: No `ServiceUrl` specified, so it uses the default template resolving to `http://localhost:8188/`

   These URLs appear in the management API responses and make service names clickable in the web dashboard for easy access to service interfaces.

9. No ListenPort in Qwen configuration makes it only available via OpenAI API.
10. Because ConsiderStoppedOnProcessExit set to true, if the process used by Qwen terminates, large-model-proxy will still continue to consider it running. This is useful if a process detaches from large-model-proxy.

With this configuration, Qwen and Automatic1111 can run at the same time. Assuming they do, a request for Gemma will unload the one least recently used. If they are currently in use, a request to Gemma will have to wait for one of the other services to free up.
`ResourcesAvailable` can include any resource metrics, CPU cores, multiple VRAM values for multiple GPUs, etc. These values are not checked against actual usage.

## Usage

```sh
./large-model-proxy -c path/to/config.json
```

If the `-c` argument is omitted, `large-model-proxy` will look for `config.json` in the current directory.

## OpenAI API endpoints

Currently, the following OpenAI API endpoints are supported:

- `/v1/completions`
- `/v1/chat/completions`
- `/v1/models` (This one makes it work with, e.g., Open WebUI seamlessly).
- `/v1/models/{model}`
- More to come

## Management API

The management API is a simple HTTP API that allows you to get the status of the proxy and the services it is proxying.

To enable it, you need to specify `ManagementApi.ListenPort` in the config.

### Web Dashboard

Access the web dashboard at `http://localhost:{ManagementApi.ListenPort}/` for a real-time view of all services, their status, resource usage, and active connections. The dashboard automatically refreshes every second and does the following:

- shows all services with their current state (running/stopped), listen ports, active connections, and last used timestamps
- when services have URLs configured, their names become clickable links that open the service's web interface in a new tab
- sorting by name, active connections, or last used time

### API Endpoints

**GET /status**: Returns a JSON object with comprehensive proxy and service information.

Response fields:

- `services`: A list of all services managed by the proxy.
- `resources`: A map of all resources managed by the proxy and their usage.

Each service in the `services` array includes the following fields:

- `name`: Service name
- `listen_port`: Port the service listens on
- `is_running`: Whether the service is currently running
- `active_connections`: Number of active connections to the service
- `last_used`: Timestamp when the service was last used (for running services)
- `service_url`: The rendered service URL (if configured), or `null` if no URL is available
- `resource_requirements`: Resources required by the service

The `service_url` field is generated from the service's `ServiceUrl` or `DefaultServiceUrl` configuration, with the `{{.PORT}}` template variable replaced by the service's `ListenPort`. The web dashboard consumes this endpoint to display real-time status information and enable clickable service navigation.

## Logs

Output from each service is logged to a separate file. Default behavior is to log it into `logs/{name}.log`,
but it can be redefined by specifying the `LogFilePath` parameter for each service.

## Contributing

Pull requests and feature suggestions are welcome.

Due to its concurrent nature, race conditions are very common in large-model-proxy, making manual testing impractical.
Therefore, I am striving to have close to 100% automated test coverage.

If you're adding a new feature, implementing automated tests for it is going to be required to merge it.

### How to run tests

Both `large-model-proxy` and `test-server/test-server` must be built before running tests.

We recommend using `make test`, which will ensure these are built before proceeding to run the tests in verbose mode.

## Contacts

I review all the issues submitted on Github, including feature suggestions.

Feel free to join my Telegram group for any feedback, questions, and collaboration:
https://t.me/large_model_proxy
