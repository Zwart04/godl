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
  <img alt="Latest release" src="https://img.shields.io/github/v/release/Zwart04/godl?label=release&color=2ea44f">
  <img alt="No npm" src="https://img.shields.io/badge/dependencies-none%20(npm)-8e8e93?labelColor=8e8e93">
</p>

<p align="center">
  English · <a href="README.id.md">Bahasa Indonesia</a>
</p>

<p align="center">
  <a href="https://github.com/Zwart04/godl/releases/latest"><img alt="Download for Windows" src="https://img.shields.io/badge/Download%20for%20Windows-godl--setup.exe-0078D6?style=for-the-badge&amp;logo=windows11&amp;logoColor=white"></a>
  <a href="https://tako.id/Zwart04"><img alt="Support the project" src="https://img.shields.io/badge/Support%20the%20project-tako.id-ff5a5f?style=for-the-badge"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#install-options">Install</a> ·
  <a href="#using-godl">Using godl</a> ·
  <a href="#browser-extension">Browser extension</a> ·
  <a href="#security-and-privacy">Security</a> ·
  <a href="#troubleshooting">Troubleshooting</a> ·
  <a href="#support-the-project">Support</a> ·
  <a href="#development">Development</a>
</p>

---

<p align="center">
  <img src="docs/screenshot.png" alt="godl download list with a running transfer and completed downloads" width="100%">
  <br>
  <sub>Sample data. godl running on Windows 11.</sub>
</p>

godl splits a file across several HTTP connections at once, so a server that
would only feed one stream is asked for several at the same time. It resumes
across restarts, keeps an honest record of every transfer, and never lets two
copies of itself fight over the same file.

It is written from scratch in Go. Nothing is derived from any existing download
manager's code, assets, or branding — the layout is familiar on purpose, the
implementation is not.

## Quick start

**What you need:** Windows 10 or 11 (64-bit). godl installs per-user and does
not need administrator rights, Go, Node.js, or npm.

> **Windows 10 note:** godl uses the Microsoft Edge WebView2 runtime. Windows 11
> includes it, and most Windows 10 machines already have it through Edge. If
> godl opens a blank window or does not open at all, install the
> [WebView2 Evergreen Runtime](https://developer.microsoft.com/microsoft-edge/webview2/consumer/).

### 1. Download the installer

Download **[`godl-setup.exe`](https://github.com/Zwart04/godl/releases/latest/download/godl-setup.exe)**
from the [latest release](https://github.com/Zwart04/godl/releases/latest).
It is one file, about 14 MB. The source for every release is in this repository.

### 2. Run the installer

Run `godl-setup.exe`. It copies the programs into
`%LOCALAPPDATA%\Programs\godl`, creates Start Menu and desktop shortcuts,
registers browser support, adds godl to Windows startup, and opens the app.

> **If Windows shows “Windows protected your PC”:** the installer is not
> code-signed yet, so SmartScreen can warn about a new, unsigned program.
> Choose **More info → Run anyway** only for a file you downloaded from this
> repository. If you prefer not to bypass the warning, [build from source](#building-it-from-source)
> or [verify the SHA-256](#verify-the-download) first.

The setup is deliberately small. It does not install a service, a driver, or a
bundled browser; the interface uses the Microsoft WebView2 runtime. To choose
another folder, skip autostart, or uninstall, see
[Install options](#install-options).

### 3. Open godl

Open **godl** from the Start Menu if the installer did not open it. The window
is the whole interface. Closing it keeps godl running in the notification area;
use **Exit** from the tray icon when you want to stop it.

Your files land in `Downloads\godl`, sorted into category folders. Download
history and settings live in `%APPDATA%\godl`.

### 4. Connect Brave (recommended)

The setup registers the browser helper and puts the extension in
`%LOCALAPPDATA%\Programs\godl\extension\chrome`. Load it once:

1. Open `brave://extensions`.
2. Turn on **Developer mode**.
3. Click **Load unpacked**.
4. Select `%LOCALAPPDATA%\Programs\godl\extension\chrome`.
5. Pin **godl** to the toolbar.

Chrome and Edge use the same folder, through `chrome://extensions` or
`edge://extensions`. Firefox has a separate folder; see
[Browser extension](#browser-extension).

### 5. Make your first download

In Brave, click a normal download link. godl opens a small dialog with the file
name, folder, size, and connection count. Change anything you need, then press
**Download**. The transfer starts in godl and the file is filed under
`Downloads\godl`.

You can also press **Add** in godl (`Ctrl`+`N`) and paste a direct `http` or
`https` file link. Start with 8 connections; godl falls back to one connection
automatically when a server does not support segmented downloads.

If a link does not behave as expected, see
[Troubleshooting](#troubleshooting). Nothing is downloaded from a copied link
or a browser handoff until you confirm it.

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
- Closing the window puts godl in the notification area and keeps downloading.
  **Exit** from the tray icon asks first when transfers are running, then leaves
  them **paused with their progress intact** rather than failed
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

**Filing**
- Seven built-in categories — Programs, Archives, Documents, Music, Video,
  Pictures and a catch-all Other — each with its own folder under the download
  directory
- A new download is filed by its extension the moment the link arrives, so a
  browser's own links need no input at all
- Change a download's category and the file moves with it, into that folder,
  without ever overwriting what is already there. A transfer under way is the
  one exception: its file is open, so the change is refused rather than half
  done
- The list is yours to change. A `categories.json` beside the rest of godl's
  state folds over the built-ins — an entry naming `.iso` makes ISOs a
  category of their own, and one with a new name simply adds it. A file that
  cannot be read falls back to the built-ins, because a settings file must
  never be able to stop downloads landing somewhere
- A category's folder is created when it is needed, including again if you
  tidy it away while downloads still point at it

**Links you copy**
- godl watches the clipboard for an address and asks before fetching anything:
  a dialog with the link in full and two buttons, and nothing is downloaded
  until one of them is pressed. Copying a URL never starts a transfer by itself
- Only a bare `http` or `https` address that makes up the whole of what was
  copied is offered. A sentence with a link in the middle is prose, and prose is
  not a file
- One copy is one offer. The next one is offered when you copy something else,
  and a link already in the list is not offered at all
- If the attempt fails, the link stays on offer rather than disappearing with
  the error - a server that could not be reached should not cost you the address
  as well as the attempt
- Reads the clipboard through Windows itself, from the process already running:
  no helper, no injection, and it is opened only for as long as the read takes
  so other programs are never held up by it
- On by default in the window, off in `--serve` unless started with
  `--clipboard`, and `godl.exe --no-clipboard` turns it off for good

**Interface**
- A native desktop window, not a browser tab: its own process, its own taskbar
  entry, its own line in Task Manager
- **A system tray icon that stays.** Closing the window hides it; double-click
  the icon or choose **Open godl** to bring it back, and **Exit** to stop the
  manager. godl keeps running and downloading while hidden
- IDM-style columnar list — name, category, size, status, speed, time left,
  connections
- **A transfer window of its own, per download.** It opens when a transfer
  starts with the address, the file size, what has arrived, the rate, the time
  left and whether it can resume; a progress bar underneath, and below that the
  map of which connection owns which part of the file with a row per connection
  saying whether it is receiving, waiting or done. Pause, Cancel and *Hide
  details* (which collapses the map and the table, and grows back to exactly
  the height it had), plus tabs for what happens when the transfer finishes and
  for a speed limit. It closes itself when the download does, and double-clicking
  a row brings it back
- Sortable columns, in-place row updates, keyboard shortcuts
- Apple-inspired visual design: hairline rules, one accent colour, no chrome
  that is not doing work

**Browser**
- Extension for Chrome, Brave, Edge and Firefox
- Catches downloads and hands them to godl with the page's cookies, referer
  and user agent, so a server that said yes to the browser says yes to godl
- **Opens the download dialog first**, with the name, the folder, the size and
  the connection count, and waits for you — *Start*, change anything, or *not
  now*. A single switch in the extension's options turns the asking off
- Gives the download straight back to the browser if godl is unavailable —
  nothing is ever lost by trying
- A desktop notification confirms a successful handoff, and says so when godl
  cannot take the link and the browser keeps the download
- Brings the window to the front when a handoff arrives, even if it was hidden
  in the tray or sitting behind the browser that sent the link

**After a download**
- Files land in your own `Downloads\godl` folder, sorted into category folders
  underneath — not inside a profile directory you would have to go looking for
- A balloon from Windows names the file and the folder it landed in. The
  browser's own row is already gone by then, and a window behind four others
  is not an announcement

**Footprint**
- **One setup file.** `godl-setup.exe` carries everything, installs it, wires
  the browser, and launches the app — there is no second file to place by hand
- No installer framework, no runtime to install, no background service
- No Node.js, no npm, no bundler — the web interface is three hand-written
  files served by the manager's own handler
- About 14 MB for the setup, roughly 22 MB once installed

## Install options

The default setup in [Quick start](#quick-start) is enough for most people.
These options are for a custom or scripted install.

The setup copies `godl.exe`, `godl-host.exe` and the browser extension into
`%LOCALAPPDATA%\Programs\godl`, creates Start Menu and desktop shortcuts,
registers itself with Chrome, Brave, Edge and Firefox, adds itself to Windows
startup, and opens the app. The extension lands in
`%LOCALAPPDATA%\Programs\godl\extension\chrome` (and `…\firefox`), which is the
folder to pick when the browser asks for one.

You never need to place the second program yourself: `godl-host.exe` is the
file your *browser* launches when a download link is clicked, and the setup
puts it where the browser has already been told to find it.

```powershell
.\godl-setup.exe                          # install and start
.\godl-setup.exe -quiet                   # install without the dialog
.\godl-setup.exe -dir "D:\Tools\godl"     # install somewhere else
.\godl-setup.exe -uninstall                # remove programs, shortcuts and browser keys
.\godl-setup.exe -no-autostart             # install, but not with Windows
.\godl-setup.exe -no-desktop               # install, but no desktop icon
```

Uninstalling removes the programs, the shortcuts, the startup entry and all
four browser registrations. Your download history in `%APPDATA%\godl` and your
files in `Downloads\godl` are left alone unless you delete them yourself.

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
Before the first download the list is empty; use **Add** or click a download link
in the browser.

<p align="center">
  <img src="docs/screenshot-empty.png" alt="godl download list before the first download" width="80%">
  <br>
  <sub>The first-run view. The list fills as transfers are added.</sub>
</p>

| | |
|---|---|
| **Add** | Opens a sheet to paste a URL, name the file, and choose a connection count. <kbd>Ctrl</kbd>+<kbd>N</kbd>. |
| **Pause / Resume** | Stops a transfer where it is, and starts it again where it stopped. Pausing holds: nothing picks the download up until you resume it. Both survive closing the app. |
| **Stop** | Abandons the transfer and discards the partial file. |
| **Limit** | Caps how fast the selected download may transfer, from 128 KB/s up or a figure of your own. It takes effect on the transfer already running, not on the next one. |
| **Arrows** | Move the selected download earlier or later in the queue. The queue starts them in that order. Pressing one drops a column sort, so you see the queue's own order — the one the arrows act on. |
| **Schedule** | Says when this download may begin. A transfer already under way is stopped and held; a paused, failed or discarded one is put back in the queue for that time. An empty box means now, which is how a schedule is cleared. If the transfer finished on its way out, the control says so rather than pretend the file is still to come. |
| **Category chip** | The category in its own column, and the way to change it. Picking another moves the file into that folder's folder. A download running right now has its file open, so its chip is off. |
| **Folder** | Opens Explorer with the finished file selected. |
| **Delete** | Removes the record. |
| **Close the window** | If transfers are running, you are asked first. In-flight ones are paused; queued ones start again on the next launch. |

**A link you copy.** Copy a bare `http` or `https` address and a sheet appears
with it in full and two buttons: **Download** puts it in the list through the
same path **Add** uses, and **Not now** puts it down. Nothing is fetched until
you choose; a copy on its own never starts a transfer. Text that merely
mentions a link is ignored, as is anything already in the list.

State lives in `%APPDATA%\godl`; finished files land in your own
`Downloads\godl`. Setting `GODL_HOME` redirects both — which is how the test
suite runs against a scratch directory, and why a test run never drops a file
into your real Downloads folder or rewrites your browser's registration.

## Browser extension

The setup puts the extension files on disk and registers the native messaging
host for Chrome, Brave, Edge, and Firefox. You only need to load the extension
into each browser once. Its source is in
[`browser-extension/`](browser-extension); the Chrome and Firefox copies use the
same logic and differ only in their manifests.

<p align="center">
  <img src="docs/capture.png" alt="godl browser capture dialog showing the address, file name, size, connection count and download button" width="420">
  <br>
  <sub>When a browser hands over a link, godl asks first. Nothing is fetched until you press Download.</sub>
</p>

### Brave, Chrome, and Edge

1. Open `brave://extensions` (or `chrome://extensions` / `edge://extensions`).
2. Turn on **Developer mode**.
3. Click **Load unpacked**.
4. Select `%LOCALAPPDATA%\Programs\godl\extension\chrome`. From a source
   checkout, use `browser-extension/chrome` instead.
5. Pin **godl** to the toolbar so you can see when it is active.

Reload the extension once after upgrading godl so the browser picks up the
`notifications` permission. A desktop notification confirms a successful
handoff, and tells you when the browser kept the download instead.

### Firefox

1. Open `about:debugging#/runtime/this-firefox`.
2. Choose **Load Temporary Add-on…**.
3. Select `%LOCALAPPDATA%\Programs\godl\extension\firefox\manifest.json`.

Firefox treats an unsigned extension as temporary: it is removed when Firefox
restarts. The Brave/Chrome/Edge route above does not have that limitation.

### Using it

Click a normal download link. godl captures it, brings its window to the front,
and asks what to do with it: the name, folder, size, connection count, and a
**Download** button. Change any of it, press **Not now**, or leave it and the
transfer begins where you said. If godl is not open, it is launched for you.

The asking is controlled by **Ask before each download** in the extension's
options. Turned off, transfers start as soon as a link is clicked, the way a
browser does it, only across eight connections instead of one. The options page
also lets you restrict capture to particular file types and set the connection
count.

If automatic capture is off, right-click a link and choose
**Download with godl**. That manual route uses the same local handoff.

### If the browser cannot find godl

The setup registers the native messaging host, and `godl.exe` rewrites the
registration on each launch. A run started with `GODL_HOME` deliberately skips
that step, because it is a test or a portable copy. If the browser reports a
missing host after moving the install, repair the keys by hand:

```powershell
& "$env:LOCALAPPDATA\Programs\godl\godl.exe" --register-browser   # rewrite the keys
& "$env:LOCALAPPDATA\Programs\godl\godl.exe" --unregister-browser # remove them
```

### Command-line switches

Every switch is optional, and every one is on the same binary:

| Switch | What it does |
| --- | --- |
| `--register-browser` | Writes the Chrome, Brave, Edge and Firefox keys |
| `--unregister-browser` | Removes them |
| `--serve` | Runs the same engine with no window; <kbd>Ctrl</kbd>+<kbd>C</kbd> stops it |
| `--clipboard` | With `--serve`, watches the clipboard the way the window does |
| `--no-clipboard` | Turns the watcher off for the window |

## Security and privacy

godl is a local Windows program. There is no account, cloud service, telemetry,
analytics, or auto-update channel. It contacts the network for the downloads you
ask for, plus the loopback handoff between the browser helper and the manager.

### The local API stays on your machine

- The control API binds to `127.0.0.1` on a random port. It is not reachable
  from your local network or the internet.
- A bearer token is generated on first run and stored at
  `%APPDATA%\godl\api-token`. Every API call must present it.
- The window is served from the same origin and receives the token in its page.
  A page on another site cannot read that document or use the API without the
  token.
- The endpoint is recorded at `%APPDATA%\godl\api-endpoint`; the browser helper
  reads it locally. Do not port-forward this endpoint or expose it through a
  proxy.

### What is stored, and where

| Path | Contents |
| --- | --- |
| `%LOCALAPPDATA%\Programs\godl` | Programs and extension files |
| `%APPDATA%\godl` | Download history, API token, endpoint, category rules |
| `Downloads\godl` | Finished files, sorted into category folders |

The installer also creates Start Menu and desktop shortcuts, adds a per-user
startup entry, and writes the browser native-messaging keys. Uninstalling
removes those, but deliberately leaves your history and downloaded files for
you to delete. See [Install options](#install-options) for flags that skip the
shortcuts or autostart.

### The browser extension

The extension source is in [`browser-extension/`](browser-extension). Its
manifest asks for `nativeMessaging`, `downloads`, `notifications`, `cookies`,
`storage`, `contextMenus`, and `<all_urls>`. The broad URL permission is needed
because a download can start on any site; it is used to notice the download and
read the cookie, referer, and user agent for that one request. Those values go
to godl through the local native-messaging pipe, not to a third-party server.

The extension is loaded unpacked, not installed from the Chrome Web Store. You
can read every line, or build and load it yourself. Firefox treats it as a
temporary add-on because it is unsigned; Brave, Chrome, and Edge keep the
unpacked extension until you remove it.

The clipboard watcher is part of the window, not the extension. It reads the
clipboard only while godl is running, offers only a bare `http`/`https` address,
and never downloads from a copy without confirmation. Start
`godl.exe --no-clipboard` to turn it off.

### Installer and SmartScreen

`godl-setup.exe` is not code-signed yet. That is why Windows SmartScreen may
show **Windows protected your PC** for a new download. The warning reflects the
missing signature and the file's reputation; it is not an antivirus verdict.
Only bypass it for a file you downloaded from
[this repository](https://github.com/Zwart04/godl), and verify the checksum if
you want certainty.

#### Verify the download

PowerShell can calculate the installer's SHA-256:

```powershell
Get-FileHash .\godl-setup.exe -Algorithm SHA256 | Format-List
```

Compare the `Hash` value with the SHA-256 in the
[latest release notes](https://github.com/Zwart04/godl/releases/latest). If it
does not match, delete the file and download it again; do not run it. The same
release notes link to the source tag that produced the installer.

If you would rather not trust a prebuilt unsigned file, build it yourself:

```powershell
git clone https://github.com/Zwart04/godl.git
cd godl
.\build-setup.ps1
```

### Downloaded files are still files from the internet

godl verifies that its own transfer is internally consistent, but it is not an
antivirus and does not scan downloaded content. Keep Microsoft Defender or
another antivirus enabled, and only open files from sources you trust.

To report a security problem, follow [SECURITY.md](SECURITY.md). Please do not
open a public issue for an exploitable vulnerability.

## Troubleshooting

### The installer is blocked by Windows

This is a SmartScreen warning about an unsigned installer. Choose **More info →
Run anyway** only after confirming the file came from this repository. If you
are not comfortable doing that, verify the SHA-256 or build from source as
described in [Security and privacy](#security-and-privacy). Do not disable
SmartScreen globally.

### godl does not open

- Make sure you are on 64-bit Windows 10 or 11.
- Install the [WebView2 Evergreen Runtime](https://developer.microsoft.com/microsoft-edge/webview2/consumer/)
  if the window is blank or never appears. Windows 11 normally includes it.
- Check the notification area: closing the window hides godl rather than
  stopping it. Double-click the tray icon to reopen it.
- If a previous copy is still running, exit it from the tray before launching
  the new one. godl allows one manager per machine.

### Brave still downloads in the browser

1. Open `brave://extensions` and confirm **godl** is loaded and enabled.
2. Open the extension's options and confirm **Ask before each download** and
   download interception are on.
3. Reload the extension after installing or upgrading godl.
4. Make sure the native host still points at the installed copy. The repair
   command is in [Browser extension](#browser-extension).
5. If godl is not running, the extension deliberately gives the download back
   to the browser instead of losing it. Start godl and try again.

The same checks apply to Chrome and Edge. Firefox's temporary add-on must be
loaded again after Firefox restarts.

### A download fails or falls back to one connection

Some servers do not support HTTP Range requests, so godl automatically uses one
connection. That is expected. A link that worked in a browser but fails in godl
may depend on a login, an expiring token, a one-time URL, or a site that blocks
non-browser clients. Re-copy the link from the page, make sure you are signed in,
or let the browser handle that particular download. godl never removes the
browser's copy of a failed handoff.

### Where are my files and history

Finished files are in `Downloads\godl`, under the category folder chosen for
each download. History, the API token, and category settings are in
`%APPDATA%\godl`. The installer and programs are in
`%LOCALAPPDATA%\Programs\godl`.

### Uninstall or stop autostart

Run the setup with `-uninstall`:

```powershell
.\godl-setup.exe -uninstall
```

This removes the programs, shortcuts, startup entry, and browser registrations.
It leaves `%APPDATA%\godl` and `Downloads\godl` intact; delete those folders
yourself if you want a completely clean removal.

To keep godl installed but stop it starting with Windows, use
`godl-setup.exe -no-autostart`, or disable the **godl** entry in Task Manager →
Startup apps. Exit godl from its tray icon before uninstalling.

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
| `internal/category` | Category list, extension rules, folder names kept inside the download directory |
| `internal/clipboard` | Reading the clipboard for a link worth offering, and deciding what counts as one |
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
| `GET` | `/api/categories` | The categories in force, in the order they are shown |
| `GET` | `/api/clipboard` | The copied link waiting for an answer, if any |
| `POST` | `/api/clipboard/accept` | Download the waiting link, through the ordinary add path |
| `POST` | `/api/clipboard/ignore` | Put the waiting link down |
| `GET` | `/api/captures` | The downloads the browser handed over and nobody has answered, oldest first |
| `POST` | `/api/captures` | Hand one over without starting it — the browser's route |
| `POST` | `/api/captures/{id}/accept` | Start it, applying whatever the dialog changed |
| `POST` | `/api/captures/{id}/ignore` | Put it down without downloading |
| `POST` | `/api/downloads` | Add a download |
| `POST` | `/api/downloads/{id}/pause` | Pause |
| `POST` | `/api/downloads/{id}/resume` | Resume |
| `POST` | `/api/downloads/{id}/retry` | Retry after a failure |
| `POST` | `/api/downloads/{id}/cancel` | Stop and discard the partial file |
| `POST` | `/api/downloads/{id}/speed-limit` | Cap this download, or clear the cap with `0` |
| `POST` | `/api/downloads/{id}/schedule` | Say when this download may begin, or clear the schedule with an empty `starts_at` |
| `POST` | `/api/downloads/{id}/move-up` | Move ahead of the download above it |
| `POST` | `/api/downloads/{id}/move-down` | Move behind the download below it |
| `POST` | `/api/downloads/{id}/category` | Put it in another category, moving its file |
| `POST` | `/api/downloads/{id}/reveal` | Show in Explorer |
| `POST` | `/api/downloads/{id}/status` | Open this download's transfer window (`204` when there is no window to show) |
| `DELETE` | `/api/downloads/{id}` | Remove the record |

```jsonc
// POST /api/downloads
{
  "url": "https://example.com/large.iso",
  "filename": "large.iso",       // optional; derived from the URL if absent
  "connections": 8,              // 1-32
  "speed_limit": 0,              // bytes per second; 0 means no cap
  "category": "Programs",        // optional; the extension decides if absent
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

Stopped is not the same as unfinished. A transfer on its way out can still
reach the end — the bytes are whole and the part file has been published — and
then there is nothing left to hold. Every control shares one rule in that
case: `409` with *"this download finished before the change could be applied"*,
and the record left `done`. Writing `queued`, `paused` or `cancelled` beside a
file that has already arrived would have the window waiting on a clock for a
download with no first byte left to wait for.

`move-up` and `move-down` swap places with a neighbour and are answered against
queue order — the order the list shows unless a column sort is over it. Off
either end they return `409`, which describes the list rather than a fault.

`category` takes a name from `/api/categories`, and an unknown one is answered
with the list it was checked against rather than a bare refusal:

```jsonc
// POST /api/downloads/0f3c1a9b7d2e4c5a/category
{ "category": "Music" }
```

The file moves with it. It is refused with `409` while the download is
transferring, because the worker holds the file open and Windows will not
rename one that is — saying so beats half-moving a transfer that is under way.

The categories themselves are the seven built-ins, folded over by a
`categories.json` kept beside the rest of godl's state (`%APPDATA%\godl`, or
`GODL_HOME` if you pointed it somewhere):

```json
[
  { "name": "ISOs", "folder": "Disc Images", "extensions": ["iso", "img"] },
  { "name": "Music", "extensions": ["mp3", "flac"] }
]
```

Only what you want changed needs to be in that file: an entry whose name
already exists replaces it, and one with a new name is added. Extensions are
claimed by the entry that names them last, so adding `ISOs` above takes `.iso`
away from Programs rather than being quietly ignored. A folder is a single
directory name — anything looking like a path is rejected, so no entry can put
a download outside the folder you chose. A file that cannot be read falls back
to the built-ins.

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

# The race detector needs cgo, and cgo needs a C compiler on PATH - MinGW-w64
# does the job. It can then be built into the program itself with
# `wails build -race`, and the suite below runs that binary end to end.
go test -race -count=1 ./...

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
.\scripts\smoke.ps1            # launch, auth, transfer, filing, clipboard, shutdown
.\scripts\test-installer.ps1   # install, shortcuts, registration, uninstall
```

`smoke.ps1` starts the real `godl.exe` with `GODL_HOME` pointed at a scratch
directory, transfers 32 MB from a local range-supporting server, and checks
that the file arrives byte-for-byte, that it is filed under its category, that
a link copied to the clipboard is only ever offered, that a second launch is
refused rather than becoming a second scheduler, and that closing the window
releases the single-instance lock. It puts the clipboard back afterwards.

`GODL_SMOKE_EXE` points that same suite at another binary, which is how a
race-enabled build is checked - `wails build -race`, giving the detector real
concurrent transfers instead of only the unit tests. A windowed program has
no console to write to, so the suite captures the manager's stderr to a file
and fails at the end if the detector ever found something.

Unit tests cover segment planning, resume and restart, concurrency limits,
state recovery, the HTTP control API, categories and folder rules, the
clipboard watcher, the native messaging protocol, and browser registration.

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
│  ├─ notify/                  the balloon Windows shows when a download ends
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
- [x] Browser extension for Chrome, Brave, Edge and Firefox
- [x] Single-file setup with install, shortcuts and uninstall
- [x] Per-download speed limit, set when adding or moved while a transfer runs
- [x] Queue ordering and a scheduled start
- [x] Download categories and folder rules
- [x] Clipboard monitor for copied links
- [x] Downloads land in your own `Downloads\godl`, with a Windows balloon naming
  the file and the folder when a transfer ends
- [x] A run redirected by `GODL_HOME` cannot touch the machine's browser
  registration
- [x] The download dialog a browser capture opens — name, folder, size and
  connection count before anything starts, with a switch to stop asking
- [x] The transfer window — a native, per-download window showing URL, size,
  received, rate, time left and resume, with the progress bar, the map of which
  connection owns which part of the file, the per-connection table, and
  pause/cancel/hide details that collapse it back to its exact height

## Support the project

godl is free and open source under the MIT license. If it saves you time and
you would like to support its development, you can do so here:

<p align="center">
  <a href="https://tako.id/Zwart04"><img alt="Support the godl project on tako.id" src="https://img.shields.io/badge/Support%20the%20project-tako.id-ff5a5f?style=for-the-badge"></a>
</p>

Support is voluntary. It does not unlock features, remove limits, or change how
the app works. Starring the repository, sharing it with someone who needs it,
and reporting a reproducible bug are just as helpful.

## License

[MIT](LICENSE) © Zwart

Written from scratch. The interface layout takes inspiration from established
download managers; no code, artwork, or branding from any of them is included.
