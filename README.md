<p align="center">
  <img src="docs/logo.png" width="104" height="104" alt="godl logo">
</p>

<h1 align="center">godl</h1>

<p align="center">
  <b>A segmented download manager for Windows.</b><br>
  Multi-connection transfers, automatic browser capture, and a native
  interface that stays out of the way.
</p>

<p align="center">
  <img alt="Go 1.27" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=ffffff&labelColor=00ADD8">
  <img alt="Windows" src="https://img.shields.io/badge/platform-Windows-0078D6?logo=windows11&logoColor=white&labelColor=0078D6">
  <img alt="Wails v2" src="https://img.shields.io/badge/UI-Wails%20v2-5c9ead?labelColor=5c9ead">
  <img alt="MIT License" src="https://img.shields.io/badge/license-MIT-34c759?labelColor=34c759">
  <img alt="No npm" src="https://img.shields.io/badge/dependencies-none%20(npm)-8e8e93?labelColor=8e8e93">
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#using-godl">Using godl</a> ·
  <a href="#browser-extension">Browser extension</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#http-api">HTTP API</a> ·
  <a href="#development">Development</a>
</p>

---

![godl window with a completed download](docs/screenshot.png)

godl splits a file across several HTTP connections at once, so a server that
would only feed one stream is asked for several at the same time. It resumes
across restarts, keeps an honest record of every transfer, and never lets two
copies of itself fight over the same file.

It is written from scratch in Go. Nothing is derived from any existing download
manager's code, assets, or branding — the layout is familiar on purpose, the
implementation is not.

## Features

**Transfer**
- Segmented parallel downloading, 1 to 32 connections per file
- Automatic fallback to a single connection when a server refuses `Range`
  requests — the connection count shown is always the one actually in use
- Pause, resume, retry, and stop, all resumable across restarts
- Queue order you can change: move a download ahead of the one above it and the
  queue starts it next, not merely the row that moved in the window
- A scheduled start, kept on the record so a transfer set for three in the
  morning begins even with the app closed
- Each connection writes its own region of one file, so segments never overlap
  and a finished part is never rewritten

**Reliability**
- One manager per machine. A single-instance lock prevents two schedulers
  from writing the same `.part` file, which is how downloads get corrupted
- Closing the window while transfers are running asks first, then leaves them
  **paused with their progress intact** rather than failed
- Resume continues from the progress already on disk, not from zero, and the
  engine refuses a resume the server will not honour rather than silently
  restarting
- **A pause is a pause.** The scheduler is willing to start a paused record —
  that is what lets a queue drain and what makes one resumable after a restart —
  so it is told apart from a transfer that simply stopped. Nothing restarts a
  download you paused until you ask it to
- A failure waits for you. It is recorded as failed rather than retried behind
  your back, so a host that cannot be reached is not probed again and again for
  as long as the app stays open
- State is persisted on every transition, so a crash costs seconds, not files
- An endpoint left behind by a crashed run is verified with a real
  authenticated health check before being believed

**Interface**
- A native desktop window, not a browser tab: its own process, its own taskbar
  entry, its own line in Task Manager
- IDM-style columnar list — name, size, status, speed, time left, connections
- Sortable columns, in-place row updates, keyboard shortcuts
- Apple-inspired visual design: hairline rules, one accent colour, no chrome
  that is not doing work

**Browser**
- Extension for Chrome, Edge and Firefox
- Catches downloads and hands them to godl with the page's cookies, referer
  and user agent, so a server that said yes to the browser says yes to godl
- Gives the download straight back to the browser if godl is unavailable —
  nothing is ever lost by trying

**Footprint**
- **One setup file.** `godl-setup.exe` carries everything, installs it, wires
  the browser, and launches the app — there is no second file to place by hand
- No installer framework, no runtime to install, no background service
- No Node.js, no npm, no bundler — the web interface is three hand-written
  files served by the manager's own handler
- About 14 MB for the setup, roughly 22 MB once installed

## Install

Grab **[`godl-setup.exe`](https://github.com/Zwart04/godl/releases/latest)**
and run it. That is the entire installation.

It copies `godl.exe` and `godl-host.exe` into
`%LOCALAPPDATA%\Programs\godl`, creates Start Menu and desktop shortcuts,
registers itself with Chrome, Edge and Firefox, adds itself to Windows
startup, and opens the app.

You never need to place the second program yourself: `godl-host.exe` is the
file your *browser* launches when a download link is clicked, and the setup
puts it where the browser has already been told to find it.

```powershell
.\godl-setup.exe                          # install and start
.\godl-setup.exe -quiet                   # install without the dialog
.\godl-setup.exe -dir "D:\Tools\godl"     # install somewhere else
.\godl-setup.exe -uninstall                # remove it completely
.\godl-setup.exe -no-autostart             # install, but not with Windows
.\godl-setup.exe -no-desktop               # install, but no desktop icon
```

Uninstalling removes the programs, the shortcuts, the startup entry and all
three browser registrations. Your download history in `%APPDATA%\godl` is left
alone unless you delete it yourself.

### Building it from source

```powershell
git clone https://github.com/Zwart04/godl.git
cd godl

.\build-setup.ps1          # -> godl-setup.exe
```

The script builds both programs, packs them, and embeds them into one windowed
executable. If you only want the programs:

```powershell
bin\wails.exe build                              # -> build\bin\godl.exe
go build -o build\bin\godl-host.exe .\cmd\godl-host
```

## Using godl

Open **godl** from the Start Menu (or the desktop icon the setup created).

| | |
|---|---|
| **Add** | Opens a sheet to paste a URL, name the file, and choose a connection count. <kbd>Ctrl</kbd>+<kbd>N</kbd>. |
| **Pause / Resume** | Stops a transfer where it is, and starts it again where it stopped. Pausing holds: nothing picks the download up until you resume it. Both survive closing the app. |
| **Stop** | Abandons the transfer and discards the partial file. |
| **Limit** | Caps how fast the selected download may transfer, from 128 KB/s up or a figure of your own. It takes effect on the transfer already running, not on the next one. |
| **Arrows** | Move the selected download earlier or later in the queue. The queue starts them in that order. Pressing one drops a column sort, so you see the queue's own order — the one the arrows act on. |
| **Schedule** | Says when this download may begin. A transfer already under way is stopped and held; a paused, failed or discarded one is put back in the queue for that time. An empty box means now, which is how a schedule is cleared. |
| **Folder** | Opens Explorer with the finished file selected. |
| **Delete** | Removes the record. |
| **Close the window** | If transfers are running, you are asked first. In-flight ones are paused; queued ones start again on the next launch. |

Everything lives in `%APPDATA%\godl`. Setting `GODL_HOME` redirects all of it
elsewhere, which is how the test suite runs against a scratch directory.

## Browser extension

The extension lives in [`browser-extension/`](browser-extension). Chrome and
Edge use `browser-extension/chrome`; Firefox uses `browser-extension/firefox`.
The logic in both is byte-identical — only the manifest differs.

**1. Register the native messaging host** — the setup does this for you, and
`godl.exe` does it again on every launch so moving the folder never breaks it.
These two commands are only for repairing it by hand:

```powershell
& "$env:LOCALAPPDATA\Programs\godl\godl.exe" --register-browser   # rewrites the keys
& "$env:LOCALAPPDATA\Programs\godl\godl.exe" --unregister-browser # removes them
```

**2. Load the extension**

- **Chrome / Edge** — open `chrome://extensions`, turn on *Developer mode*,
  choose *Load unpacked*, and pick `browser-extension/chrome`.
- **Firefox** — open `about:debugging#/runtime/this-firefox`, choose
  *Load Temporary Add-on…*, and pick `browser-extension/firefox/manifest.json`.

**3. Use it.** Click any download link. godl captures it, and if the app was
not open it is launched for you.

Right-click → *Download with godl* is available as a manual route whenever
automatic capture is switched off in the extension's options. The options page
also lets you limit capture to particular file types and set the connection
count.

## How it works

```
   browser ──stdio──▶ godl-host.exe ──HTTP──▶ ┌────────────────────────┐
                                              │       godl.exe        │
   double-click ─────────────────────────────▶│                        │
                                              │  queue  ─▶  engine    │
                                              │  api    ─▶  state     │
                                              └────────────────────────┘
                                                        │
                                                        ▼
                                                  files on disk
```

The browser cannot hand a file to a download manager by itself. It launches a
small helper — a *native messaging host* — listed in a per-user registry key
that points at a manifest on disk. `godl-host.exe` is that helper: it speaks
the browser's framing on stdin/stdout, and godl's local HTTP API on the other
side.

If no manager is running when a link is clicked, `godl-host.exe` launches
`godl.exe` and waits for it. The download proceeds either way.

The desktop window is not a separate program. It is `godl.exe` showing
WebView2 against the *same* control API the browser host uses — one queue, one
token, one place where a button can be wrong.

| Package | Responsibility |
|---|---|
| `internal/engine` | Segmented transfer, range planning, speed accounting, SHA-256 verification |
| `internal/queue` | Scheduling, concurrency limits, status transitions |
| `internal/state` | On-disk records, atomic writes, recovery on start |
| `internal/api` | The loopback HTTP control plane and the served interface |
| `internal/host` | The native messaging protocol and its client |
| `internal/nmh` | Native messaging frame encoding — 4-byte length prefix, JSON body |
| `internal/browsersetup` | The three registry keys and two manifests |
| `internal/app` | Assembling a manager, the single-instance lock, startup and shutdown |

## HTTP API

A bearer token is created on first run and stored at
`%APPDATA%\godl\api-token`. Every request needs it. The server binds to
`127.0.0.1` only — it is never reachable from the network.

| Method | Path | |
|---|---|---|
| `GET` | `/api/health` | Liveness check |
| `GET` | `/api/downloads` | Every record with live progress |
| `POST` | `/api/downloads` | Add a download |
| `POST` | `/api/downloads/{id}/pause` | Pause |
| `POST` | `/api/downloads/{id}/resume` | Resume |
| `POST` | `/api/downloads/{id}/retry` | Retry after a failure |
| `POST` | `/api/downloads/{id}/cancel` | Stop and discard the partial file |
| `POST` | `/api/downloads/{id}/speed-limit` | Cap this download, or clear the cap with `0` |
| `POST` | `/api/downloads/{id}/schedule` | Say when this download may begin, or clear the schedule with an empty `starts_at` |
| `POST` | `/api/downloads/{id}/move-up` | Move ahead of the download above it |
| `POST` | `/api/downloads/{id}/move-down` | Move behind the download below it |
| `POST` | `/api/downloads/{id}/reveal` | Show in Explorer |
| `DELETE` | `/api/downloads/{id}` | Remove the record |

```jsonc
// POST /api/downloads
{
  "url": "https://example.com/large.iso",
  "filename": "large.iso",       // optional; derived from the URL if absent
  "connections": 8,              // 1-32
  "speed_limit": 0,              // bytes per second; 0 means no cap
  "cookie": "session=…",         // forwarded verbatim
  "referer": "https://…",        // forwarded verbatim
  "user_agent": "…"              // forwarded verbatim
}
```

`speed-limit` takes the same field and is accepted in any state — including one
that is transferring right now, which is the reason it exists:

```jsonc
// POST /api/downloads/0f3c1a9b7d2e4c5a/speed-limit
{ "speed_limit": 1048576 }       // 1 MB/s; 0 removes the cap
```

`schedule` takes an RFC 3339 instant, or an empty string to mean "now", which
is how a schedule is cleared:

```jsonc
// POST /api/downloads/0f3c1a9b7d2e4c5a/schedule
{ "starts_at": "2026-10-20T03:00:00Z" }
```

It is accepted everywhere but a finished download, and it puts the record back
in the queue — a time only means something to a transfer the queue is willing
to start. A download already under way is stopped first, with its progress
kept, because it has already begun and the only honest reading of "start it at
nine" is "stop it now, start it at nine". `resume` and `retry` drop the
schedule, since being told to go now is a newer answer to *when?* than the one
on the record.

`move-up` and `move-down` swap places with a neighbour and are answered against
queue order — the order the list shows unless a column sort is over it. Off
either end they return `409`, which describes the list rather than a fault.

Progress is reported as:

```jsonc
{
  "id": "0f3c1a9b7d2e4c5a",
  "status": "running",
  "segmented": true,
  "connections": 8,
  "speed_limit": 1048576,
  "starts_at": "2026-10-20T03:00:00Z",   // absent unless scheduled
  "total_size": 33554432,
  "progress": { "done": 16777216, "total": 33554432, "percent": 50.0, "speed": 4123568.0 }
}
```

## Development

```powershell
$env:PATH = "C:\Program Files\Go\bin;" + $env:PATH   # if Go is not on PATH

gofmt -l .                     # must print nothing
go vet ./...                   # must exit 0
go test -count=3 -shuffle=on ./...

# The installer is behind a build tag, so the checks above skip it by
# design; check it separately once a payload exists.
.\build-setup.ps1
go vet -tags setup ./cmd/godl-setup
```

### End-to-end verification

Two scripts exercise the built programs rather than the source. Both clean up
after themselves even when a check fails, so one red run cannot poison the
next.

```powershell
.\scripts\smoke.ps1            # 29 checks: launch, auth, transfer, shutdown
.\scripts\test-installer.ps1   # 24 checks: install, shortcuts, registration, uninstall
```

`smoke.ps1` starts the real `godl.exe` with `GODL_HOME` pointed at a scratch
directory, transfers 32 MB from a local range-supporting server, and checks
that the file arrives byte-for-byte, that a second launch is refused rather
than becoming a second scheduler, and that closing the window releases the
single-instance lock.

There are 52 unit tests covering segment planning, resume and restart,
concurrency limits, state recovery, the HTTP control API, the native messaging
protocol, and browser registration.

Set `GODL_HOME` to a scratch directory to run against a throwaway
configuration:

```powershell
$env:GODL_HOME = "$env:TEMP\godl-dev"
& "$env:LOCALAPPDATA\Programs\godl\godl.exe"
```

### Project layout

Every executable is built, never committed — `build-setup.ps1` produces them
from the source below.

```
godl/
├─ main.go                     the window: size, close confirmation, flags
├─ notify_windows.go           message boxes (a GUI app has no console)
├─ build-setup.ps1             builds both programs and packs the setup
├─ cmd/
│  ├─ godl-setup/              installer: extracts, registers, shortcuts
│  └─ godl-host/               the browser's native messaging host
├─ internal/
│  ├─ api/                     control API + web/ (index.html, app.css, app.js)
│  ├─ app/                     manager assembly, single-instance lock
│  ├─ browsersetup/            registry keys and manifests
│  ├─ config/                  directories, token, endpoint file
│  ├─ engine/                  segmented transfer
│  ├─ host/                    native messaging client
│  ├─ nmh/                     native messaging framing
│  ├─ queue/                   scheduler
│  └─ state/                   on-disk records
├─ browser-extension/
│  ├─ chrome/                  Chrome and Edge
│  └─ firefox/                 Firefox (identical logic)
├─ scripts/                    end-to-end verification
├─ build/
│  ├─ appicon.png              icon source
│  ├─ bin/                     build outputs (gitignored)
│  └─ windows/                 icon.ico and the Windows manifest
└─ docs/                       logo and screenshots
```

## Roadmap

- [x] Segmented parallel transfer with automatic single-connection fallback
- [x] Pause, resume, retry, stop — all surviving restart
- [x] Single-instance guarantee and graceful shutdown
- [x] Native desktop window
- [x] Local HTTP control API with token auth
- [x] Native messaging host with automatic manager startup
- [x] Browser extension for Chrome, Edge and Firefox
- [x] Single-file setup with install, shortcuts and uninstall
- [x] Per-download speed limit, set when adding or moved while a transfer runs
- [x] Queue ordering and a scheduled start
- [ ] Download categories and folder rules
- [ ] Clipboard monitor for copied links

## License

[MIT](LICENSE) © Zwart

Written from scratch. The interface layout takes inspiration from established
download managers; no code, artwork, or branding from any of them is included.
