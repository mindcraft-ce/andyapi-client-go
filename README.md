# AndyAPI Local Provider Client

Bridge your local LLMs (OpenAI-compatible) to the AndyAPI server over WebSocket, with a tiny built-in management UI for setup, model discovery, and connection control.

- Lightweight Go binary (no external DB)
- WebSocket heartbeat + exponential reconnect
- Configurable provider name and per-model capabilities
- Local model discovery via `/v1/models` (Ollama, vLLM, OpenAI-compatible APIs)
- Forwards chat completions to your local API (`/v1/chat/completions`)
- Built-in UI at `http://localhost:8090/ui` by default

---

## Quick start

1) Build or run

- Run directly (requires Go 1.22+):

```fish
cd client
go run .
```

- Or build for your OS:

```fish
cd client
go build -o andyapi-client
./andyapi-client
```

2) Open the UI

- Visit http://localhost:8090/ui
- If no `config.yaml` exists, the client starts in “initial setup” mode.

3) Fill in settings

- Andy API Base URL (example: `http://localhost:8080` or `https://api.example.com`)  → the client derives `ws(s)://…/ws`
- Optional Provider name (default: `provider`)
- Local OpenAI API URL (for model scan and completions), e.g. `http://localhost:11434`
- Optional Local OpenAI API Key (if your local endpoint requires it)

4) Discover and enable models

- Click “Scan from Local API” to populate models from `GET {local_api_url}/v1/models`
- Toggle “Enabled” for models you want to serve
- Save

5) Connect to AndyAPI

- Click “Connect to AndyAPI” in the header.
- Status badge shows: never connected → connected/disconnected

---

## What it does

```
AndyAPI  ⇄  WebSocket (/ws)
   ⇅           ↑ heartbeat/ping
Client
   ⇅ request/response
Local OpenAI-compatible API (e.g. Ollama, vLLM)
        ↳ /v1/chat/completions
```

- On connect, the client registers the models you enabled.
- Incoming requests from AndyAPI are forwarded to your local API’s `/v1/chat/completions` with the requested `model` and `max_tokens` (when provided).
- Responses are streamed back (non-streaming payload today; first choice content is returned).

---

## Configuration

Location: `client/config.yaml` (auto-created/saved from the UI). Example fields:

```yaml
andy_api_url: "http://localhost:8080"  # Base; ws(s) URL is derived as ws(s)://host/ws
andy_api_key: ""                        # Reserved for future auth
provider: "local-llm"                   # Your provider label
heartbeat_interval: 30                   # Seconds
reconnect_max_backoff: 30                # Seconds
local_api_url: "http://localhost:11434" # OpenAI-compatible base (without /v1)
local_api_key: ""                       # Optional bearer for local API
models:
  - name: "local-7b-chat"
    max_completion_tokens: 4096
    concurrent_connections: 2
    supports_embedding: false
    supports_vision: false
    enabled: true
```

Notes
- `andy_api_url` can be `http(s)://…` or `ws(s)://…`; if `http(s)://`, the client derives `ws(s)://…/ws` automatically.
- Model discovery uses `GET {local_api_url}/v1/models` and heuristically marks embedding/vision when names include keywords like “embed”, “vision”, or “vl”.

---

## Management UI and API

UI routes
- `GET /ui` – SPA for configuration and controls

Health and state
- `GET /health` – `{ status, client_id, ws_url }`
- `GET /api/status` – `{ connected, ever_connected, client_id, ws_url }`
- `GET /api/state` – `{ initial_setup }`
- `GET /config` – Current config as JSON

Connection control
- `POST /api/connect` – Start background WS connect loop
- `POST /api/disconnect` – Stop loop and close the socket

Config endpoints
- `POST /api/save-config` – Only during initial setup; saves full config
- `POST /api/update-config` – Update config after setup (URL, provider, local API, models, etc.)
- `POST /models` – Replace models (array) and broadcast update to server
- `POST /api/scan-models` – Body: `{ base_url, api_key }`; responds `{ models: [...] }`

Default listen address: `:8090` (change with `-http` flag).

---

## Command-line flags

- `-config <path>` – Path to config YAML (default `config.yaml`)
- `-example <path>` – Example config to load if `-config` missing (default `config.example.yaml`)
- `-http <addr>` – Management HTTP listen address (default `:8090`)

Examples

```fish
# Custom config and port
./andyapi-client -config ./my-config.yaml -http :8089
```

---

## Build options

Simple build

```fish
cd client
go build -o andyapi-client
```

Cross-platform build script

- Script: `client/build.sh` (bash)
- Outputs to `client/dist/<goos>-<goarch>/`
- Variables:
  - `VERSION` (default: `dev`)
  - `TARGETS` (default: `"windows/amd64 linux/amd64"`)
  - `CLEAN` (default: `false`)
  - `RACE` (default: `false`)
  - `VERBOSE` (default: `false`)

Example

```fish
cd client
env VERSION=0.1.0 TARGETS="linux/amd64" bash ./build.sh
```

Artifacts include a binary, `SHA256SUMS`, and a `.tar.gz` bundle.

---

## Troubleshooting

- Not connecting
  - Check AndyAPI base URL; HTTPS → WSS, HTTP → WS (client derives `/ws`).
  - Verify the server exposes `/ws` and is reachable from this machine.
  - See `/api/status` and logs for errors; adjust `heartbeat_interval`/`reconnect_max_backoff`.

- Model scan fails
  - Ensure `local_api_url` is correct and reachable; it must expose `/v1/models`.
  - If your local endpoint requires a key, set `local_api_key`.

- Completions fail
  - Ensure the `model` name exists in your local endpoint and supports chat completions.
  - The client uses the first choice’s message content from `/v1/chat/completions` responses.

- Port in use
  - Change the management address via `-http :<port>`.

---

## Security notes

- API keys are stored locally in `config.yaml`; secure file permissions as needed.
- When exposing the UI remotely, protect the port with firewall/auth (no built-in auth yet).
- Prefer `https://` for AndyAPI so the client uses `wss://`.

---

## Requirements

- Go 1.22 or newer (to build from source)
- A local OpenAI-compatible API for inference:
  - Examples: Ollama, vLLM, LM Studio, OpenWebUI in OpenAI proxy mode
- A public OpenAI-compatible API for inference (Optional):
  - Examples: OpenRouter, OpenAI, TogetherAI, Gemini

---

## License

This client is part of the AndyAPI project. See the repository’s main license for details.
