# Security policy

## Supported versions

Security fixes are provided for the latest release of godl. If you are running an
older build, update from the [latest release](https://github.com/Zwart04/godl/releases/latest)
before reporting a problem.

## Reporting a vulnerability

Please report security problems privately through GitHub's
[private vulnerability reporting](https://github.com/Zwart04/godl/security/advisories/new).
Do not open a public issue for a vulnerability that could put users at risk.

A useful report includes:

- the godl version and how it was installed;
- the Windows version and browser;
- clear reproduction steps or a proof of concept;
- the impact you believe the issue has;
- any relevant logs, with tokens, cookies, and personal paths removed.

This is a small project, so responses are best effort. I aim to acknowledge a
report within seven days and will coordinate disclosure after a fix is
available.

## Scope

This policy covers:

- `godl.exe`, `godl-host.exe`, and `godl-setup.exe`;
- the Chrome/Brave/Edge and Firefox extensions in `browser-extension/`;
- the local control API and the native-messaging bridge.

The following are generally out of scope:

- vulnerabilities in Windows, WebView2, Chromium, Brave, Chrome, Edge, or
  Firefox themselves;
- malicious downloads, phishing pages, or files that the user chooses to open;
- attacks that require code execution or administrative access as the same
  Windows user already running godl;
- denial of service caused by a local process or by the remote download server;
- missing hardening that has no realistic security impact.

## Security model

godl is a per-user Windows program. It has no account system, cloud service,
telemetry, or analytics, and it never downloads or installs an update on its
own. Its network traffic is the download request itself, an optional release
check against GitHub, plus the loopback connection between the browser helper
and the manager.

### Optional release check

- After start-up, and only when the user presses **Check for updates**, godl may
  request `https://api.github.com/repos/Zwart04/godl/releases/latest`.
- The request carries the installed godl version in its `User-Agent`. It does
  not include an account, cookies, download history, file names, or local paths.
  As with any HTTPS request, GitHub can see the connecting IP address.
- The response is used only to compare version numbers and to offer a link to
  the release page. No installer is downloaded, executed, or run automatically.
- `--no-update-check` disables both the startup check and the on-demand check,
  including in `--serve` mode.

### Local API

- The HTTP API listens on `127.0.0.1` with an operating-system-assigned port.
  It is not bound to a LAN interface and must not be exposed through a proxy or
  port forward.
- A random bearer token is created on first run and stored at
  `%APPDATA%\godl\api-token`. The file is created with mode `0600` inside the
  per-user `%APPDATA%` profile, and every API request must present the token.
- The endpoint is recorded in `%APPDATA%\godl\api-endpoint`. The browser helper
  reads that file locally instead of guessing a port.
- The UI is served from the same origin and receives the token in its page. A
  cross-origin page cannot read that document or use the API without the token.
- A process running as the same Windows user may be able to read the token and
  the user's files. That is the normal same-user boundary; godl is not a
  sandbox.

### Browser extension

The extension uses the native-messaging host to pass a download to godl. It
asks for `nativeMessaging`, `downloads`, `notifications`, `cookies`, `storage`,
`contextMenus`, and `<all_urls>`. The broad URL permission is required because a
download can start on any site. The extension uses it to observe the chosen
download and read its URL, referer, user agent, and cookies; those values are
sent only to godl over the local pipe. The extension does not upload browsing
history or download activity to a third-party service.

The extension is loaded unpacked from `%LOCALAPPDATA%\Programs\godl\extension`
or from a source checkout. It is not distributed through the Chrome Web Store.
Review the source before loading it if you need to verify its behavior.

The separate clipboard watcher runs inside `godl.exe`. It reads the clipboard
only while the program is running, offers only a bare `http` or `https` URL, and
does not start a transfer without confirmation. It can be disabled with
`godl.exe --no-clipboard`.

### Installer and binaries

- The installer is per-user: it writes under `%LOCALAPPDATA%`, `%APPDATA%`, the
  current user's registry hive, and the Start Menu. It does not install a
  service or a driver.
- It creates shortcuts, adds a startup entry, and writes browser
  native-messaging registrations. `-no-desktop` skips the desktop shortcut and
  `-no-autostart` skips the startup entry; `-uninstall` removes the programs,
  shortcuts, startup entry, and browser registrations.
- The binaries are not code-signed. Windows SmartScreen may warn about them.
  Users who need stronger assurance should verify the release SHA-256 or
  [build from source](README.md#building-it-from-source).
- Release checksums are published in the release notes. Compare them with
  `Get-FileHash .\godl-setup.exe -Algorithm SHA256`. If the hash does not match,
  do not run the file.

### Downloaded files

godl verifies that its own transfer is internally consistent, but it is not an
antivirus, a reputation service, or a sandbox. It does not scan downloaded
content. Users should keep Microsoft Defender or another antivirus enabled and
should only open files from sources they trust.

## Safe harbor

Good-faith security research is welcome. Please avoid privacy violations,
service disruption, and access to data that is not yours, and give the project
a reasonable chance to fix an issue before publishing details.
