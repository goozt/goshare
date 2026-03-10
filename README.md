# GoShare

GoShare is a tiny peer-to-peer file sharing web server written in Go. It exposes a simple web UI (served from `public/`) that lets clients on the same network upload, download and delete files from a shared directory. Transfers are instrumented with WebSocket-based progress notifications so the browser can show a live upload/download percentage.

## Features

- Single-binary, embed static assets with `embed.FS` (no build step for the UI)
- List, delete files in a directory
- `POST` multipart uploads with progress
- Download with progress reporting and proper `Content-Disposition`
- WebSocket hub for push notifications
- Basic panic recovery middleware
- Discover local IPs for quick network access

## Getting started

### Prerequisites

- Go 1.16+ (modules-enabled)

### Building

```sh
go build -o goshare ./...
```

This produces a `goshare` executable in the current directory.

### Running

By default the server will use `~/Downloads/shared` as the directory to store files and listen on port 8080.

```sh
# use defaults
go run main.go

# specify a custom directory and port
./goshare -d /path/to/shared -p 9000
```

When started it prints the URLs you can use from other machines on the same LAN:
```
GoShare starting...
Shared directory: /home/user/Downloads/shared
Listening on http://0.0.0.0:8080
  → http://192.168.1.23:8080
  → http://10.0.0.5:8080
```

Open any of those addresses in a browser (mobile or desktop) to view the front-end.

### CLI flags

| Flag | Description | Default |
|------|-------------|---------|
| `-d` | Shared directory | `$HOME/Downloads/shared` |
| `-p` | Port to listen on | `8080` |

### Browser client

The `public/` directory contains the static web UI. It uses vanilla JavaScript to talk to `/api` endpoints and the WebSocket `/ws` path for progress messages. You do not need to rebuild anything when modifying the front end; the files are embedded automatically by the `//go:embed public` directive.

### API

- `GET /api/files` – list files
- `DELETE /api/files/{name}` – delete file
- `POST /api/upload?clientId={id}` – multipart form upload (field `file`) with progress notifications over WS
- `GET /api/download/{name}?clientId={id}` – download file with progress notifications
- `GET /ws` – WebSocket endpoint; on connect the server replies with `{type:"connected",clientId:"<id>"}`

### Security / sandboxing

Only base names are allowed for uploads/downloads; path traversal attempts are rejected. The shared directory is created automatically and confined.

## Development notes

- Static assets are embedded via `embed.FS`. To add or change HTML/CSS/JS make edits in `public/` then simply rebuild Go; no additional tooling required.
- The progress messages are defined by the `ProgressMsg` struct in `main.go`.

## License

This project is released under the MIT license.

```text
MIT License

<copyright holder>
```

Replace `<copyright holder>` with your name if you distribute this.
