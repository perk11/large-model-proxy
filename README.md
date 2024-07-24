# Large Model Proxy

Large Model Proxy is designed to make it easy to run multiple resource-heavy Large Models (LM) on the same machine with limited amount of VRAM/other resources.
 It listens on a dedicated port for each proxied LM, making them always available to the clients connecting to these ports.


## How it works

Upon receiving a connection on a specific port, if LM is not already started, it will:

1. Verify if the required resources are available to start the corresponding LM.
2. If resources are not available, it will automatically stop the least recently used LM to free up contested resources.
3. Start LM.
4. Wait for LM to be available on the specified port.
5. Wait for 2 seconds (This will be replaced with a support for a proper healthcheck). 

Then it will proxy the connection from the LM to the client that connected to it.
To the client this should be fully transparent with the only exception being that receiving any data on the connection takes longer if LM had to be spun up. 

## Installation
**Ubuntu and Debian**: Download the deb file attached to the [latest release](https://github.com/perk11/large-model-proxy/releases/latest).

**Arch Linux**: Install from [AUR](https://aur.archlinux.org/packages/large-model-proxy).

**Other Distros**:
1. Install go

3. Clone the repository:
    ```sh
    git clone https://github.com/perk11/large-model-proxy.git
    ```
4. Navigate into the project directory:
    ```sh
    cd large-model-proxy
    ```
5. Build the project:
    ```sh
    go build -o large-model-proxy main.go
    ```
    or
   ```sh
    make
    ```

## Configuration

Below is an example config.json:
```json
{
  "MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds": 1200,
  "ResourcesAvailable": {
    "VRAM-GPU-1": 24000,
    "RAM": 32000
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
      "RestartOnConnectionFailure": true,
      "ResourceRequirements": {
        "VRAM-GPU-1": 6000,
        "RAM": 30000
      }
    },
    {
      "Name": "assistant",
      "ListenPort": "8081",
      "ProxyTargetHost": "localhost",
      "ProxyTargetPort": "18081",
      "Command": "/opt/llama.cpp/llama-server",
      "Args": "-m /opt/Gemma-27B-v1_Q4km.gguf -c 8192 -ngl 100 -t 4 --port 18081",
      "RestartOnConnectionFailure": false,
      "ResourceRequirements": {
        "VRAM-GPU-1": 20000,
        "RAM": 3000
      }
    }
  ]
}
```
This configuration will run automatic1111's Stable Diffusion web UI on port 7860 and llama.cpp on port 8080.
large-model-proxy will expect these services to be available on port 17860 and 18080 once started.
It allocates up to 24GB of VRAM and 32GB of RAM for them.
The Stable Diffusion web UI is expected to use up to 6GB of VRAM and 30GB of RAM, while llama.cpp will use up to 20GB of VRAM and 3GB of RAM.

"ResourcesAvailable" can include any resource metrics, CPU cores, multiple VRAM values for multiple GPUs, etc. these values are not checked against actual usage.

## Usage
```sh
./large-model-proxy -c path/to/config.json
```

If `-c` argument is omitted, `large-model-proxy` will look for `config.json` in current directory

## Logs

Output from each service is logged to a separate file. Default behavior is to log it into logs/{name}.log,
but it can be redefined by specifying `LogFilePath` parameter for each service.
