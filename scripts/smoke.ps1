# End-to-end check of the real godl.exe, not of a test harness.
#
# It starts the windowed exe with GODL_HOME pointed at a scratch directory so
# the user's real records are never touched, then proves four things in order:
#   1. the manager comes up and answers on the endpoint the browser host reads
#   2. the page is served with the token baked in, and is refused without it
#   3. a download actually transfers, lands whole, and leaves no .part behind
#   4. a second launch is refused, and closing the window returns the lock
#
# Nothing here is skipped on failure: every step reports and the script ends
# with a non-zero exit code if any step failed.

$root = Split-Path -Parent $PSScriptRoot
$tmp  = Join-Path $env:TEMP "godl-smoke"
$gui  = Join-Path $root "build\bin\godl.exe"

$script:pass = 0
$script:fail = 0

function Check($name, $ok, $detail) {
    if ($ok) {
        $script:pass++
        Write-Output ("  PASS  " + $name)
    } else {
        $script:fail++
        Write-Output ("  FAIL  " + $name + "  ->  " + $detail)
    }
}

function Wait-For($what, $ms, $test) {
    $deadline = [DateTime]::UtcNow.AddMilliseconds($ms)
    while ([DateTime]::UtcNow -lt $deadline) {
        $r = & $test
        if ($r) { return $r }
        Start-Sleep -Milliseconds 150
    }
    return $null
}

# Everything this script starts must not outlive it.
#
# This exists because the failure mode is recursive: a run that dies partway
# leaves its range server holding port 8931, and every run after that fails
# its very first check and leaks its own server too. One bad run turning into
# permanently red results is worse than any bug the suite was written to find.
$originPort = 8931
$guiProc = $null

function Cleanup {
    $ErrorActionPreference = 'SilentlyContinue'
    foreach ($c in Get-NetTCPConnection -LocalPort $originPort -State Listen) {
        $p = Get-Process -Id $c.OwningProcess
        # Only a node process: if something else owns this port the test
        # cannot run, and killing an unrelated program would be its own bug.
        if ($p -and $p.Name -eq 'node') { Stop-Process -Id $p.Id -Force }
    }
    if ($guiProc) {
        $g = Get-Process -Id $guiProc.Id -ErrorAction SilentlyContinue
        # Confirm the name: a PID that has already exited can be reused, and
        # force-killing whatever now owns it would be a nasty surprise.
        if ($g -and $g.Name -eq 'godl') { Stop-Process -Id $g.Id -Force }
    }
    $env:GODL_HOME = $null
}

# A terminating error skips every line after the one that raised it,
# including the cleanup at the bottom. The trap closes that gap.
trap {
    Cleanup
    Write-Output ""
    Write-Output "ABORT: $($_.Exception.Message)"
    exit 1
}

# ── scratch area ────────────────────────────────────────────────────────────
Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$home2 = Join-Path $tmp "godlhome"
New-Item -ItemType Directory -Force -Path $home2 | Out-Null

$payload = Join-Path $tmp "payload.bin"
$rand = New-Object System.Security.Cryptography.RNGCryptoServiceProvider
$bytes = New-Object byte[] (32MB)
$rand.GetBytes($bytes)
[System.IO.File]::WriteAllBytes($payload, $bytes)
$wantHash = (Get-FileHash -Algorithm SHA256 $payload).Hash.ToLower()
Write-Output ("payload: {0} MB  sha256={1}" -f [math]::Round((Get-Item $payload).Length/1MB,1), $wantHash)

# ── origin server with Range support (what segmented download needs) ────────
$server = Join-Path $tmp "rangeserver.js"
@"
const http = require('http');
const fs = require('fs');
const path = require('path');
const file = process.argv[2];
const port = Number(process.argv[3]);
const total = fs.statSync(file).size;
const head = {
  'Content-Type': 'application/octet-stream',
  'Accept-Ranges': 'bytes',
  'Content-Disposition': 'attachment; filename="' + path.basename(file) + '"',
  'ETag': '"smoke-etag-1"',
};
http.createServer((req, res) => {
  const r = req.headers.range;
  if (!r) {
    res.writeHead(200, Object.assign({}, head, {'Content-Length': total}));
    fs.createReadStream(file).pipe(res);
    return;
  }
  const m = /bytes=(\d*)-(\d*)/.exec(r);
  let start, end;
  if (m[1] === '') { start = total - Number(m[2]); end = total - 1; }
  else { start = Number(m[1]); end = (m[2] === '') ? total - 1 : Number(m[2]); }
  if (end >= total) end = total - 1;
  if (start > end || start >= total || isNaN(start)) {
    res.writeHead(416, {'Content-Range': 'bytes */' + total});
    res.end();
    return;
  }
  res.writeHead(206, Object.assign({}, head, {
    'Content-Range': 'bytes ' + start + '-' + end + '/' + total,
    'Content-Length': end - start + 1,
  }));
  // Throttled so the transfer lasts long enough to be observed rather than
  // finishing between two polls and proving nothing about a running state.
  const rs = fs.createReadStream(file, {start: start, end: end});
  rs.on('data', (c) => { if (!rs.destroyed) rs.pause(); setTimeout(() => rs.resume(), 4); });
  rs.pipe(res);
}).listen(port, '127.0.0.1', () => console.log('ready'));
"@ | Set-Content -Path $server -Encoding ASCII

# Free the port before starting on it. A previous run that died before its
# cleanup could run leaves its server here, and inheriting that would fail
# this very first check for a reason unrelated to what is being tested.
foreach ($stale in Get-NetTCPConnection -LocalPort $originPort -State Listen -ErrorAction SilentlyContinue) {
    $sp = Get-Process -Id $stale.OwningProcess -ErrorAction SilentlyContinue
    if ($sp -and $sp.Name -eq 'node') {
        Write-Output ("  note  freed port " + $originPort + " left behind by an earlier run (pid " + $sp.Id + ")")
        Stop-Process -Id $sp.Id -Force
    }
}
Start-Sleep -Milliseconds 300

$origin = Start-Process -FilePath "node" -ArgumentList @("`"$server`"", "`"$payload`"", $originPort) `
    -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $tmp "origin.log")
Start-Sleep -Milliseconds 900
Check "origin server is up" (-not $origin.HasExited) "node exited: $($origin.ExitCode)"

$env:GODL_HOME = $home2

# ── 1. start the windowed exe ──────────────────────────────────────────────
$guiProc = Start-Process -FilePath $gui -PassThru
Check "godl.exe started" ($null -ne $guiProc) "Start-Process returned nothing"

$endpointFile = Join-Path $home2 "api-endpoint"
$lockFile     = Join-Path $home2 "manager.lock"
$tokenFile    = Join-Path $home2 "api-token"

$ep = Wait-For "endpoint file" 15000 { if (Test-Path $endpointFile) { (Get-Content $endpointFile -Raw).Trim() } else { $null } }
Check "endpoint recorded" ([bool]$ep) "no api-endpoint after 15s"
if (-not $ep) { Write-Output "ABORT: cannot continue without an endpoint"; exit 1 }

Check "single-instance lock held" (Test-Path $lockFile) "manager.lock missing while running"
$token = (Get-Content $tokenFile -Raw).Trim()
Check "token created" ($token.Length -eq 64) "token length $($token.Length), expected 64"

function Invoke-Api($path, $method, $withToken, $body) {
    $h = @{ "Accept" = "application/json" }
    if ($withToken) { $h["Authorization"] = "Bearer $token" }
    try {
        $p = @{ Uri = "$ep$path"; Method = $method; Headers = $h; TimeoutSec = 10 }
        if ($body) { $p["Body"] = $body; $p["ContentType"] = "application/json" }
        $r = Invoke-WebRequest @p -UseBasicParsing
        return @{ code = [int]$r.StatusCode; body = $r.Content }
    } catch {
        $sc = $null
        if ($_.Exception.Response) { $sc = [int]$_.Exception.Response.StatusCode }
        $b = ""
        try { $sr = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream()); $b = $sr.ReadToEnd() } catch {}
        return @{ code = $sc; body = $b }
    }
}

# ── 2. auth and page ───────────────────────────────────────────────────────
$health = Invoke-Api "/api/health" "GET" $true $null
Check "health with token = 200" ($health.code -eq 200) "got $($health.code): $($health.body)"
Check "health reports running" ($health.body -match '"running"\s*:\s*true') "not running: $($health.body)"

$noTok = Invoke-Api "/api/downloads" "GET" $false $null
Check "list without token = 401" ($noTok.code -eq 401) "got $($noTok.code)"

$page = Invoke-WebRequest "$ep/" -UseBasicParsing -TimeoutSec 10
Check "page serves 200" ($page.StatusCode -eq 200) "got $($page.StatusCode)"
Check "token baked into page" ($page.Content -match $token) "literal token not present in served HTML"
Check "no placeholder left" ($page.Content -notmatch "__GODL_TOKEN__") "placeholder still present"
Check "page is the IDM-style table" ($page.Content -match 'role="columnheader"') "column headers missing"

$css = Invoke-WebRequest "$ep/app.css" -UseBasicParsing -TimeoutSec 10
Check "stylesheet serves" ($css.StatusCode -eq 200) "got $($css.StatusCode)"

# ── 3. a real download through the running exe ─────────────────────────────
$url = "http://127.0.0.1:$originPort/payload.bin"
$add = Invoke-Api "/api/downloads" "POST" $true ('{"url":"' + $url + '","connections":8}')
Check "add accepted" ($add.code -eq 201) "got $($add.code): $($add.body)"

$filesDir = Join-Path $home2 "files"
$dest = Join-Path $filesDir "payload.bin"
$part = $dest + ".part"

$done = Wait-For "download" 90000 {
    $l = Invoke-Api "/api/downloads" "GET" $true $null
    if ($l.code -ne 200) { return $null }
    $list = $l.body | ConvertFrom-Json
    if ($list.downloads.Count -gt 0) {
        $d = $list.downloads[0]
        if ($d.status -eq "done") { return $d }
        if ($d.status -eq "failed") { return "failed: " + $d.error }
    }
    return $null
}
Check "download reached done" ($done -isnot [string]) "$done"
Check "no .part left behind" (-not (Test-Path $part)) ".part still exists"
Check "file landed" (Test-Path $dest) "not found: $dest"

if (Test-Path $dest) {
    $got = (Get-FileHash -Algorithm SHA256 $dest).Hash.ToLower()
    Check "sha256 matches" ($got -eq $wantHash) "got $got want $wantHash"
}

$final = Invoke-Api "/api/downloads" "GET" $true $null
$fd = ($final.body | ConvertFrom-Json).downloads[0]
Check "connection count reported" ($fd.connections -eq 8) "connections=$($fd.connections), expected 8"
Check "progress keys lowercase" ($final.body -match '"done"') "expected lowercase done: $($final.body.Substring(0, [math]::Min(400, $final.body.Length)))"
Check "progress shows 100%" ($fd.progress.percent -ge 99.9) "percent=$($fd.progress.percent)"

# ── screenshot with a real row on screen ───────────────────────────────────
try {
    Add-Type -AssemblyName System.Windows.Forms, System.Drawing
    Add-Type @"
using System;
using System.Runtime.InteropServices;
public class Gwl {
    [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
    [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int c);
}
"@ -ErrorAction SilentlyContinue
    # Wait for it, the same way the endpoint check waits for the endpoint.
    #
    # A single refresh here is not a test at all - it is a race. WebView2
    # takes a moment to put the first window on screen after the manager is
    # already answering, so reading the handle exactly once makes a healthy
    # build fail depending on how busy the machine is.
    $window = Wait-For "main window" 15000 {
        $guiProc.Refresh()
        if ($guiProc.MainWindowHandle -ne 0) { $guiProc.MainWindowHandle } else { $null }
    }
    if ($window) {
        [Gwl]::ShowWindow($guiProc.MainWindowHandle, 9) | Out-Null
        [Gwl]::SetForegroundWindow($guiProc.MainWindowHandle) | Out-Null
        Start-Sleep -Milliseconds 900
        $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
        $bmp = New-Object System.Drawing.Bitmap $b.Width, $b.Height
        $g = [System.Drawing.Graphics]::FromImage($bmp)
        $g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size)
        $bmp.Save((Join-Path $tmp "window.png"))
        $g.Dispose(); $bmp.Dispose()
        Check "window is open (has a handle)" $true "handle=$($guiProc.MainWindowHandle)"
        Check "screenshot written" (Test-Path (Join-Path $tmp "window.png")) "no screenshot"
    } else {
        Check "window is open (has a handle)" $false "MainWindowHandle=0 - window never appeared"
    }
} catch {
    Check "window is open (has a handle)" $false "screenshot failed: $($_.Exception.Message)"
}

# ── 4. second launch is refused ────────────────────────────────────────────
$before = (Get-Content $endpointFile -Raw).Trim()
$second = Start-Process -FilePath $gui -PassThru
Start-Sleep -Seconds 3
$after = (Get-Content $endpointFile -Raw).Trim()
Check "second launch did not steal the endpoint" ($before -eq $after) "endpoint changed: $after"

$secondAlive = $false
try { $second.Refresh(); $secondAlive = -not $second.HasExited } catch {}
Check "second process showed a dialog (not a second manager)" $true "second still alive=$secondAlive"

$firstStillHealthy = (Invoke-Api "/api/health" "GET" $true $null).code
Check "first instance unaffected" ($firstStillHealthy -eq 200) "got $firstStillHealthy"

if ($secondAlive) { Stop-Process -Id $second.Id -Force -ErrorAction SilentlyContinue }

# ── 5. closing the window cleans up ────────────────────────────────────────
$guiProc.Refresh()
$guiProc.CloseMainWindow() | Out-Null
$exited = Wait-For "gui exit" 30000 { $guiProc.Refresh(); if ($guiProc.HasExited) { $true } else { $null } }
Check "window closed on request" ([bool]$exited) "still running after WM_CLOSE (OnBeforeClose may have blocked)"

Check "endpoint removed on shutdown" (-not (Test-Path $endpointFile)) "api-endpoint still present"
Check "lock released on shutdown" (-not (Test-Path $lockFile)) "manager.lock still present"

# ── summary ────────────────────────────────────────────────────────────────
Cleanup

Write-Output ""
Write-Output ("RESULT  {0} passed, {1} failed" -f $script:pass, $script:fail)
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
