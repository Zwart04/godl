# End-to-end check of the real godl.exe, not of a test harness.
#
# It starts the windowed exe with GODL_HOME pointed at a scratch directory so
# the user's real records are never touched, then proves four things in order:
#   1. the manager comes up and answers on the endpoint the browser host reads
#   2. the page is served with the token baked in, and is refused without it
#   3. a download actually transfers, lands whole, and leaves no .part behind,
#      filed by extension - a link copied to the clipboard is only ever
#      offered, never fetched on its own - and the transfer's own window
#      opens with its numbers in it, closes when asked, and reopens on request
#   4. a second launch is refused, and closing the window returns the lock
#
# Nothing here is skipped on failure: every step reports and the script ends
# with a non-zero exit code if any step failed.

$root = Split-Path -Parent $PSScriptRoot
$tmp  = Join-Path $env:TEMP "godl-smoke"
$gui  = Join-Path $root "build\bin\godl.exe"
# Any other build can be put through the same suite by naming it. That is how
# a race-enabled binary is checked: a detector that never sees a real transfer
# under real concurrency has the interesting half of the program left out.
if ($env:GODL_SMOKE_EXE) { $gui = $env:GODL_SMOKE_EXE }

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
# The clipboard belongs to every program on the desktop. Section 3e borrows it
# and puts back what it found, so both are recorded before anything is touched.
$script:hadClip = $null
$script:clipTouched = $false
# The browser tables belong to the machine. A run launched with a scratch
# GODL_HOME used to rewrite them anyway, pointing the real browser at a
# manifest that was deleted along with the scratch directory - which unhooked
# a perfectly good install and left nothing on screen to explain it. That is
# now forbidden in main.go, and the check at the end of this run is what keeps
# it forbidden. Whatever is there before is put back after, so the run cannot
# leave a trace either way.
$script:browserKeys = @(
    'HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.godl.host',
    'HKCU:\Software\Chromium\NativeMessagingHosts\com.godl.host',
    'HKCU:\Software\Microsoft\Edge\NativeMessagingHosts\com.godl.host',
    'HKCU:\Software\Mozilla\NativeMessagingHosts\com.godl.moz')
$script:savedKeys = @{}
$script:keysTouched = $false
foreach ($k in $script:browserKeys) {
    $v = (Get-ItemProperty -Path $k -ErrorAction SilentlyContinue).'(default)'
    if ($null -ne $v) { $script:savedKeys[$k] = $v }
}

function Cleanup {
    $ErrorActionPreference = 'SilentlyContinue'
    if ($script:clipTouched) {
        # Text only: an image cannot be carried in a variable, and a test that
        # overwrote one should say so rather than claim it put everything back.
        try {
            if ($null -eq $script:hadClip) { Set-Clipboard -Value '' }
            else { Set-Clipboard -Value $script:hadClip }
        } catch { }
    }
    if ($script:keysTouched) {
        foreach ($k in $script:browserKeys) {
            if ($script:savedKeys.ContainsKey($k)) {
                New-Item -Path $k -Force | Out-Null
                New-ItemProperty -Path $k -Name '(default)' -Value $script:savedKeys[$k] -PropertyType String -Force | Out-Null
            } else {
                # It was not registered before, and leaving it registered now
                # would be this run's own registration wearing the user's.
                Remove-Item -Path $k -Recurse -Force
            }
        }
    }
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
  //
  // A request for "slow" is throttled far harder. One download on this origin
  // takes about a second, which is short enough that a schedule sent a few
  // milliseconds after the add could still arrive behind it - and then the
  // server would be right to refuse, having nothing left to hold. Slowing that
  // one keeps the question honest: what has a download waiting for an hour
  // transferred?
  const pause = req.url.indexOf('hold') !== -1 ? 300 : (req.url.indexOf('slow') !== -1 ? 60 : 4);
  const rs = fs.createReadStream(file, {start: start, end: end});
  rs.on('data', (c) => { if (!rs.destroyed) rs.pause(); setTimeout(() => rs.resume(), pause); });
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
# The launch below must leave the machine's browser tables alone; the values
# saved at the top are read back at the end to prove it.
$script:keysTouched = $true
# A windowed program has no console to write to, so anything it says on stderr
# - and in a race-enabled build, a race report - vanishes without this. The
# check at the end reads the file back.
$guiLog = Join-Path $tmp "godl.stderr.log"
Remove-Item $guiLog -Force -ErrorAction SilentlyContinue
$guiProc = Start-Process -FilePath $gui -PassThru -RedirectStandardError $guiLog
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
Check "page offers a speed limit control" ($page.Content -match 'id="t-limit"') "limit button missing from the served page"
Check "page offers queue ordering" ($page.Content -match 'id="t-up"') "move-up button missing from the served page"
Check "page offers a schedule control" ($page.Content -match 'id="t-schedule"') "schedule button missing from the served page"
Check "page offers the update notice" ($page.Content -match 'id="update-notice"') "update notice missing from the served page"
Check "page offers the support sheet" ($page.Content -match 'id="support"') "support sheet missing from the served page"

$updateStatus = Invoke-Api "/api/update" "GET" $true $null
Check "update status answers" ($updateStatus.code -eq 200) "got $($updateStatus.code): $($updateStatus.body)"
Check "update status is enabled" ($updateStatus.body -match '"enabled"\s*:\s*true') "not enabled: $($updateStatus.body)"
Check "update status names the installed version" ($updateStatus.body -match '"current"\s*:\s*"[^"]+"') "no current version: $($updateStatus.body)"

$css = Invoke-WebRequest "$ep/app.css" -UseBasicParsing -TimeoutSec 10
Check "stylesheet serves" ($css.StatusCode -eq 200) "got $($css.StatusCode)"

# ── 3. a real download through the running exe ─────────────────────────────
$url = "http://127.0.0.1:$originPort/payload.bin?hold=1"
$add = Invoke-Api "/api/downloads" "POST" $true ('{"url":"' + $url + '","connections":8}')
Check "add accepted" ($add.code -eq 201) "got $($add.code): $($add.body)"

# ── 3b. a per-download speed limit, set while the transfer is in flight ─────
$listed = Invoke-Api "/api/downloads" "GET" $true $null
$capId = (($listed.body | ConvertFrom-Json).downloads[0]).id
Check "download has an id" ([bool]$capId) "no id in list: $($listed.body)"

$none = Invoke-Api "/api/downloads" "GET" $true $null
$noneFirst = (($none.body | ConvertFrom-Json).downloads[0]).speed_limit
Check "new download starts uncapped" ($noneFirst -eq 0) "speed_limit=$noneFirst, expected 0"

# A 32 MB file over loopback lands in well under a second, and a transfer's
# window closes itself the moment the transfer ends - so the window checks
# below would be racing the download rather than examining the dialog. A low
# ceiling keeps the transfer alive for the length of those checks; it is lifted
# again as soon as they are done.
$held = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":32768}'
Check "transfer held open for the window checks" ($held.code -eq 200) "got $($held.code): $($held.body)"

# ── 3f. the transfer's own window ───────────────────────────────────────────
# A running download opens a window of its own - the IDM-style sheet with the
# URL, the rate, the time left and what each connection is carrying. It is
# found here by class rather than by title because the title is under test: a
# window whose caption leads with no percentage is not reporting anything.
#
# This sits immediately after the download starts on purpose. The window is a
# report on a transfer in flight and closes itself when the transfer ends, so
# checking it after a run of API calls would be racing the download rather than
# testing the dialog.
Add-Type @"
using System;
using System.Runtime.InteropServices;
using System.Text;
public class GodlStatusWin {
    public delegate bool EnumProc(IntPtr h, IntPtr l);
    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumProc cb, IntPtr l);
    [DllImport("user32.dll")]
    public static extern bool EnumThreadWindows(uint threadId, EnumProc cb, IntPtr l);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern IntPtr FindWindowEx(IntPtr parent, IntPtr after, string className, string windowName);
    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetClassName(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")]
    public static extern bool IsWindow(IntPtr h);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll")]
    public static extern IntPtr GetForegroundWindow();
    // FindWindow is deliberately not used: PowerShell marshals a null title as
    // an empty string, and FindWindow then matches only an empty caption - so
    // it finds the taskbar and never the dialog. Enumeration by class name has
    // no such trap.
    public static IntPtr FindByClass(string cls) {
        IntPtr found = IntPtr.Zero;
        EnumWindows(delegate(IntPtr h, IntPtr l) {
            StringBuilder sb = new StringBuilder(256);
            GetClassName(h, sb, sb.Capacity);
        if (sb.ToString() == cls && IsWindowVisible(h)) { found = h; return false; }
            return true;
        }, IntPtr.Zero);
        return found;
    }
    public static IntPtr FindByClassForProcess(string cls, int processId, bool visibleOnly) {
        // Tray host uses HWND_MESSAGE and therefore is absent from EnumWindows.
        if (cls == "GodlTrayWindow") {
            IntPtr messageParent = new IntPtr(-3);
            IntPtr messageWindow = FindWindowEx(messageParent, IntPtr.Zero, cls, null);
            if (messageWindow != IntPtr.Zero) {
                uint pid; GetWindowThreadProcessId(messageWindow, out pid);
                if (pid == (uint)processId) return messageWindow;
        }
        }
        IntPtr found = IntPtr.Zero;
        EnumWindows(delegate(IntPtr h, IntPtr l) {
            uint pid; GetWindowThreadProcessId(h, out pid);
            if (pid != (uint)processId) return true;
            StringBuilder sb = new StringBuilder(256);
            GetClassName(h, sb, sb.Capacity);
            if (sb.ToString() == cls && (!visibleOnly || IsWindowVisible(h))) { found = h; return false; }
            return true;
        }, IntPtr.Zero);
        return found;
    }
    public static IntPtr FindVisibleForProcess(int processId) {
        IntPtr found = IntPtr.Zero;
        EnumWindows(delegate(IntPtr h, IntPtr l) {
            uint pid; GetWindowThreadProcessId(h, out pid);
            if (pid == (uint)processId && IsWindowVisible(h)) { found = h; return false; }
            return true;
        }, IntPtr.Zero);
        return found;
    }
}
"@ -ErrorAction SilentlyContinue

function Get-StatusWindow {
    $h = [GodlStatusWin]::FindByClass("GodlStatusWindow")
    if ($h -eq [IntPtr]::Zero) { return $null }
    $sb = New-Object System.Text.StringBuilder 512
    [void][GodlStatusWin]::GetWindowText($h, $sb, $sb.Capacity)
    return @{ h = $h; title = $sb.ToString() }
}

$sw = Wait-For "transfer window" 10000 { Get-StatusWindow }
Check "a running transfer opens its own window" ([bool]$sw) "no GodlStatusWindow within 10s of the transfer starting"
Check "its title leads with the percent" ($sw -and $sw.title -match '^\d+(\.\d+)?% godl') "title was '$($sw.title)'"

# The connection table exists only if the engine reports per-segment state, so
# the list is where that claim is checked end to end: the dialog draws from
# the very numbers this row has to carry.
$seg = Invoke-Api "/api/downloads" "GET" $true $null
$segRow = (($seg.body | ConvertFrom-Json).downloads | Where-Object { $_.id -eq $capId })[0]
$segCount = @($segRow.progress.segments).Count
Check "the list carries per-connection state" ($segCount -gt 0) "progress.segments absent or empty for a segmented transfer"

# Closing that window is a desktop act, not a queue action. The transfer keeps
# running, and asking for it again the way a double-click in the list does
# brings the window straight back - which is the whole reason the reopen
# endpoint exists.
#
# A missing window is reported, not fatal: the rest of the suite still has to
# run, and a suite that dies here hides every result after it.
$closedWin = $null
if ($sw) {
    $null = [GodlStatusWin]::PostMessage($sw.h, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)   # WM_CLOSE
    $closedWin = Wait-For "transfer window close" 5000 { if ([GodlStatusWin]::IsWindow($sw.h)) { $null } else { $true } }
}
Check "the window closes when asked" ([bool]$closedWin) "GodlStatusWindow still open after WM_CLOSE (or never opened)"

$afterClose = Invoke-Api "/api/downloads" "GET" $true $null
$still = (($afterClose.body | ConvertFrom-Json).downloads | Where-Object { $_.id -eq $capId })[0].status
Check "closing it did not stop the transfer" ($still -eq 'running' -or $still -eq 'done') "status was '$still'"

$re = Invoke-Api ("/api/downloads/" + $capId + "/status") "POST" $true $null
Check "asking again reaches the window" ($re.code -eq 200) "got $($re.code): $($re.body)"
$back = Wait-For "transfer window reopen" 5000 { Get-StatusWindow }
Check "the window comes back" ([bool]$back) "no GodlStatusWindow after POST /status"

$noSuch = Invoke-Api "/api/downloads/nope/status" "POST" $true $null
Check "unknown id gets no window" ($noSuch.code -eq 404) "got $($noSuch.code)"

# The window is a report on a transfer that is still running: the checks below
# drive the same transfer, so they must not find it finished already.
Check "the transfer is still running for the limit checks" ($still -eq 'running') "status was '$still'"

# The ceiling was only there to keep the dialog on screen; the transfer has a
# suite to finish.
$lifted = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":0}'
Check "ceiling lifted after the window checks" ($lifted.code -eq 200) "got $($lifted.code): $($lifted.body)"

# The transfer is running right now, which is the case the control exists for.
$cap = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":1048576}'
Check "limit accepted on a running transfer" ($cap.code -eq 200) "got $($cap.code): $($cap.body)"

$capped = Invoke-Api "/api/downloads" "GET" $true $null
$capNow = (($capped.body | ConvertFrom-Json).downloads | Where-Object { $_.id -eq $capId })[0].speed_limit
Check "running transfer reports the new limit" ($capNow -eq 1048576) "speed_limit=$capNow, expected 1048576"

# A negative speed cannot be honoured; refusing it beats storing a number the
# engine will quietly ignore.
$neg = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":-1}'
Check "negative limit refused" ($neg.code -eq 400) "got $($neg.code)"
$unknown = Invoke-Api "/api/downloads/nope/speed-limit" "POST" $true '{"speed_limit":1024}'
Check "unknown id refused" ($unknown.code -eq 404) "got $($unknown.code)"

# Clearing it must not be mistaken for leaving it alone.
$clear = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":0}'
Check "limit can be cleared" ($clear.code -eq 200) "got $($clear.code): $($clear.body)"

# And set it again so the transfer finishes under a cap - which is what proves
# a throttled file still completes whole rather than stalling.
$again = Invoke-Api ("/api/downloads/" + $capId + "/speed-limit") "POST" $true '{"speed_limit":1048576}'
Check "limit set again" ($again.code -eq 200) "got $($again.code): $($again.body)"

$filesDir = Join-Path $home2 "files"
# Taken from the manager rather than built here. A file whose extension no
# category claims lands in the catch-all, and a path assembled by the test
# would be asserting its own guess instead of the shipped behaviour.
$dest = ($add.body | ConvertFrom-Json).download.dest
$part = $dest + ".part"
Check "unclaimed extension filed under Other" ($dest -match '[\\/]Other[\\/]') "dest=$dest"
Check "file stays under the download directory" ($dest.StartsWith($filesDir)) "dest=$dest, dir=$filesDir"

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

# The worker saves the record once more when it finishes, from a copy it made
# before the limit was set. If that copy is not refreshed the completion write
# puts the old value back - and because the transfer is over, nothing is left
# running to make the loss visible. This is the check that catches it.
Check "speed limit survived completion" ($fd.speed_limit -eq 1048576) "speed_limit=$($fd.speed_limit) after done, expected 1048576"

# ── 3c. a scheduled start, and where a download sits in the queue ───────────
#
# A second download exists only so there is something to arrange: a list of
# one has no order to change, and the refusals below would prove nothing. It
# asks for the origin's slow lane, because what these lines ask - what has a
# download waiting for an hour transferred? - only has one honest answer while
# that download is still going. On the fast lane the whole file arrives in
# about a second, so the schedule could land behind it and the server would be
# right to refuse to hold a download that had already arrived.
$add2 = Invoke-Api "/api/downloads" "POST" $true ('{"url":"' + $url + '?slow=1","connections":2}')
Check "second download accepted" ($add2.code -eq 201) "got $($add2.code): $($add2.body)"
$added2 = ($add2.body | ConvertFrom-Json).download
$secondId = $added2.id
Check "second download has an id" ([bool]$secondId) "no id: $($add2.body)"

# An hour away. The scheduler must hold it rather than treat it as work with
# nowhere to go - which is also what keeps Start from sitting on a request
# until the clock comes round.
$when = (Get-Date).ToUniversalTime().AddHours(1).ToString("yyyy-MM-ddTHH:mm:ssZ")
$sch = Invoke-Api ("/api/downloads/" + $secondId + "/schedule") "POST" $true ('{"starts_at":"' + $when + '"}')
Check "schedule accepted on a queued download" ($sch.code -eq 200) "got $($sch.code): $($sch.body)"

# Several scheduler passes rather than one, so the hold is a rule and not a
# gap between writing the record and reading it again.
Start-Sleep -Seconds 2
$heldBody = (Invoke-Api "/api/downloads" "GET" $true $null).body
$second = ($heldBody | ConvertFrom-Json).downloads | Where-Object { $_.id -eq $secondId }
Check "scheduled download still queued" ($second.status -eq "queued") "status=$($second.status), expected queued"
Check "scheduled download kept its time" ([bool]$second.starts_at) "starts_at missing from: $heldBody"
# This one is worth spelling out when it fails. "The file exists" says nothing
# on its own: it could be this download having run early, or it having finished
# before the schedule was applied, or two records sharing a name because the
# first one's file was not on disk yet when the second was added. The folder
# and the record as the server sees it now tell those apart.
$listing = (@(Get-ChildItem (Split-Path -Parent $added2.dest) -File -ErrorAction SilentlyContinue) |
    ForEach-Object { "$($_.Name)=$($_.Length)" }) -join " "
$live2 = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads |
    Where-Object { $_.id -eq $secondId }
Check "scheduled download has not transferred" (-not (Test-Path $added2.dest)) `
    "dest=$($added2.dest) status=$($live2.status) done=$($live2.progress.done)/$($live2.progress.total) starts_at=$($live2.starts_at) part=$(Test-Path ($added2.dest + '.part')) files: $listing"

# A schedule the server cannot parse must be refused rather than stored as the
# zero value, which would start the transfer at once and read as success.
$badSch = Invoke-Api ("/api/downloads/" + $secondId + "/schedule") "POST" $true '{"starts_at":"tonight"}'
Check "unparseable schedule refused" ($badSch.code -eq 400) "got $($badSch.code)"

# Where it sits is visible in the next list, because that is what the arrows
# in the window act on.
$mv = Invoke-Api ("/api/downloads/" + $secondId + "/move-up") "POST" $true ''
Check "move-up accepted" ($mv.code -eq 200) "got $($mv.code): $($mv.body)"
$orderNow = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads
Check "the moved download is first" ($orderNow[0].id -eq $secondId) "first is $($orderNow[0].id), want $secondId"
Check "the other download followed it" ($orderNow[1].id -eq $fd.id) "second is $($orderNow[1].id), want $($fd.id)"

# Off both ends: a statement about the list's shape, so 409 rather than a
# failure of the server.
$atHead = Invoke-Api ("/api/downloads/" + $secondId + "/move-up") "POST" $true ''
Check "moving past the head refused" ($atHead.code -eq 409) "got $($atHead.code)"
$atFoot = Invoke-Api ("/api/downloads/" + $fd.id + "/move-down") "POST" $true ''
Check "moving past the foot refused" ($atFoot.code -eq 409) "got $($atFoot.code)"

# Clearing the schedule is what lets it go - and it has to be seen to go, or
# the check above would pass for a schedule that simply never applied.
$clr = Invoke-Api ("/api/downloads/" + $secondId + "/schedule") "POST" $true '{"starts_at":""}'
Check "schedule can be cleared" ($clr.code -eq 200) "got $($clr.code): $($clr.body)"
$released = Wait-For "scheduled download to start" 10000 {
    $d = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads |
        Where-Object { $_.id -eq $secondId }
    if ($d -and @('running', 'done') -contains $d.status) { $d.status } else { $null }
}
Check "cleared schedule released the download" ([bool]$released) "still queued after 10s"

# Let it finish so that nothing is in flight when the window is asked to
# close - closing with a transfer running is a different code path, and the
# check further down is about a clean shutdown.
$finished2 = Wait-For "second download to finish" 40000 {
    $d = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads |
        Where-Object { $_.id -eq $secondId }
    if ($d -and $d.status -eq 'done') { 'done' } else { $null }
}
Check "scheduled download completed whole" ([bool]$finished2) "did not reach done in 40s"
Check "scheduled download landed" (Test-Path $added2.dest) "no file at $($added2.dest)"

# ── 3d. categories sort files into folders ──────────────────────────────────
$dlDir = ((Invoke-Api "/api/health" "GET" $true $null).body | ConvertFrom-Json).download_dir
Check "download directory reported" ([bool]$dlDir) "health named no download_dir"

$catsBody = (Invoke-Api "/api/categories" "GET" $true $null).body
$catNames = @(($catsBody | ConvertFrom-Json).categories | ForEach-Object { $_.name })
Check "categories are served" ($catNames.Count -ge 2) "got $($catNames.Count): $catsBody"
Check "the catch-all is last" ($catNames[-1] -eq "Other") "last was $($catNames[-1])"

# The window draws this, so it has to be there before anything is clicked.
Check "page offers a category column" ($page.Content -match 'class="th cat"') "no category header in the served page"
Check "page offers the category sheet" ($page.Content -match 'id="ct-ok"') "no category sheet in the served page"

# A name that says what it is has to land where that says it goes. The label
# alone would not be worth anything.
$musicAdd = Invoke-Api "/api/downloads" "POST" $true ('{"url":"http://127.0.0.1:' + $originPort + '/song.mp3","filename":"song.mp3","connections":2}')
Check "music download accepted" ($musicAdd.code -eq 201) "got $($musicAdd.code): $($musicAdd.body)"
$musicRec = ($musicAdd.body | ConvertFrom-Json).download
Check "music download is categorised" ($musicRec.category -eq "Music") "category=$($musicRec.category)"
Check "music download filed under Music" ($musicRec.dest -match '[\\/]Music[\\/]') "dest=$($musicRec.dest)"
# The folder is joined to a path the user chose, so a name that escaped it
# would put a download somewhere else on the machine entirely.
Check "category folder stays under the download directory" ($musicRec.dest.StartsWith($dlDir)) "dest=$($musicRec.dest), dir=$dlDir"

# A category that is not on the list is a request to file something where no
# rule exists. It is refused before the server is contacted, which is why this
# costs no transfer.
$badCat = Invoke-Api "/api/downloads" "POST" $true ('{"url":"http://127.0.0.1:' + $originPort + '/weird.xyz","filename":"weird.xyz","category":"Things"}')
Check "unknown category refused" ($badCat.code -eq 400) "got $($badCat.code): $($badCat.body)"

# Changing one has to move the file. A list saying Music while the file sat in
# Programs would be the bug this avoids - but only once nothing holds it open.
$musicDone = Wait-For "music download to finish" 40000 {
    $d = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads |
        Where-Object { $_.id -eq $musicRec.id }
    if ($d -and $d.status -eq 'done') { 'done' } else { $null }
}
Check "music download completed" ([bool]$musicDone) "did not reach done in 40s"

$mvCat = Invoke-Api ("/api/downloads/" + $musicRec.id + "/category") "POST" $true '{"category":"Documents"}'
Check "category change accepted" ($mvCat.code -eq 200) "got $($mvCat.code): $($mvCat.body)"
$musicAfter = ($mvCat.body | ConvertFrom-Json).download
Check "category change recorded" ($musicAfter.category -eq "Documents") "category=$($musicAfter.category)"
Check "category change moved the file" ($musicAfter.dest -match '[\\/]Documents[\\/]') "dest=$($musicAfter.dest)"
Check "the moved file exists" (Test-Path $musicAfter.dest) "not found: $($musicAfter.dest)"
Check "the old file is gone" (-not (Test-Path $musicRec.dest)) "still at $($musicRec.dest)"

# ── 3e. a copied link is offered, never taken ───────────────────────────────
$script:hadClip = $null
try { $script:hadClip = Get-Clipboard -Raw -ErrorAction Stop } catch { $script:hadClip = $null }
$script:clipTouched = $true

function Put-Clipboard($text) {
    # godl reads the clipboard once a second and holds it only for the read,
    # but a collision lands on whichever of the two got there first. Retrying
    # turns a lost race into a delay instead of an abort.
    for ($i = 0; $i -lt 20; $i++) {
        try { Set-Clipboard -Value $text; return $true } catch { Start-Sleep -Milliseconds 100 }
    }
    return $false
}

function Clipboard-Url {
    $b = (Invoke-Api "/api/clipboard" "GET" $true $null).body
    try {
        $j = $b | ConvertFrom-Json
        return [string]$j.url
    } catch {
        return $null
    }
}

Check "clipboard endpoint answers" ((Invoke-Api "/api/clipboard" "GET" $true $null).code -eq 200) "GET /api/clipboard did not answer 200"
Check "nothing is offered yet" ((Clipboard-Url) -eq "") "already offering: $(Clipboard-Url)"

# The sheet has to be in the page before it can ever be opened.
Check "page offers the copied-link sheet" ($page.Content -match 'id="cl-yes"') "no copied-link sheet in the served page"

$copied = "http://127.0.0.1:$originPort/copied.bin"
Check "a link can be put on the clipboard" (Put-Clipboard $copied) "Set-Clipboard failed 20 times"

$seen = Wait-For "the copied link to be offered" 6000 { $u = Clipboard-Url; if ($u) { $u } else { $null } }
Check "a copied link is offered" ($seen -eq $copied) "offered '$seen', want '$copied'"

# Accepting goes down the same path the Add button takes - the same probe, the
# same validation, the same filing by extension.
$acc = Invoke-Api "/api/clipboard/accept" "POST" $true $null
Check "the copied link is accepted" ($acc.code -eq 201) "got $($acc.code): $($acc.body)"
$copiedRec = ($acc.body | ConvertFrom-Json).download
Check "the copy was filed by its extension" ($copiedRec.category -eq "Other") "category=$($copiedRec.category)"
Check "the offer is given up once taken" ((Clipboard-Url) -eq "") "still offering: $(Clipboard-Url)"

$copiedDone = Wait-For "the copied download to finish" 90000 {
    $d = ((Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json).downloads |
        Where-Object { $_.id -eq $copiedRec.id }
    if ($d -and $d.status -eq 'done') { 'done' } else { $null }
}
Check "the copied download completed" ([bool]$copiedDone) "did not reach done in 90s"
# Guarded: a refusal above would leave this null, and Test-Path refuses a null
# path outright - which would abort the run instead of failing one check.
Check "the copied download landed" ($copiedRec -and (Test-Path $copiedRec.dest)) "not found: $($copiedRec.dest)"

# Nothing waiting: accepting must say so rather than invent an empty download.
$none = Invoke-Api "/api/clipboard/accept" "POST" $true $null
Check "accepting when nothing is waiting says so" ($none.code -eq 404) "got $($none.code)"

# A second copy is a second offer.
$copied2 = "http://127.0.0.1:$originPort/copied-too.bin"
Put-Clipboard $copied2 | Out-Null
$seen2 = Wait-For "the second copy to be offered" 6000 { $u = Clipboard-Url; if ($u) { $u } else { $null } }
Check "a second copy is offered too" ($seen2 -eq $copied2) "offered '$seen2', want '$copied2'"

# Not now puts it down - and saying so twice is not a fault, because the window
# is clearing what it was showing rather than reporting one.
$ign = Invoke-Api "/api/clipboard/ignore" "POST" $true $null
Check "a copied link can be dismissed" ($ign.code -eq 200) "got $($ign.code)"
Check "the dismissed link is gone" ((Clipboard-Url) -eq "") "still offering: $(Clipboard-Url)"
$ign2 = Invoke-Api "/api/clipboard/ignore" "POST" $true $null
Check "dismissing nothing is not an error" ($ign2.code -eq 200) "got $($ign2.code)"

# Prose that mentions an address is not an address. The wait has to outlast a
# watcher tick, or "not yet" would be counted as "not ever".
Put-Clipboard "see https://example.com/for-it" | Out-Null
Start-Sleep -Milliseconds 3500
Check "prose is not offered" ((Clipboard-Url) -eq "") "offered: $(Clipboard-Url)"

# ── the download wizard a browser capture opens ────────────────────────────
# A capture is a question. Before this existed the manager answered it
# itself, and the suite would never have noticed: the file landed, the
# checksum matched, and nobody had been asked anything.
$capURL  = "http://127.0.0.1:$originPort/captured.bin"
$capAdd  = Invoke-Api "/api/captures" "POST" $true ('{"url":"' + $capURL + '","filename":"captured.bin","connections":2,"cookie":"session=topsecret"}')
Check "capture accepted" ($capAdd.code -eq 201) "got $($capAdd.code): $($capAdd.body)"

$capID = ''
try { $capID = ($capAdd.body | ConvertFrom-Json).capture.id } catch {}
Check "capture has an id" ([bool]$capID) "no capture.id in $($capAdd.body)"

$capBefore = (Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json
Check "nothing transferred before anyone chose" (-not (@($capBefore.downloads) | Where-Object { $_.url -eq $capURL })) "the capture started on its own"

$capList = (Invoke-Api "/api/captures" "GET" $true $null).body | ConvertFrom-Json
Check "the wizard has something to show" (@($capList.captures).Count -eq 1) "got $(@($capList.captures).Count)"
Check "the capture list keeps its secrets" (-not ($capList | ConvertTo-Json -Compress).Contains("topsecret")) "a session cookie reached the page"

$capAcc  = Invoke-Api "/api/captures/$capID/accept" "POST" $true '{"filename":"renamed.bin","connections":2}'
Check "answering starts the download" ($capAcc.code -eq 201) "got $($capAcc.code): $($capAcc.body)"

$capDest = ''
try { $capDest = ($capAcc.body | ConvertFrom-Json).download.dest } catch {}
Check "the name chosen in the dialog is used" ($capDest.EndsWith('renamed.bin')) "dest=$capDest"

$capDone = Wait-For "captured download" 30000 {
    $d = (Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json
    $hit = @($d.downloads) | Where-Object { $_.url -eq $capURL }
    if ($hit -and $hit.status -eq 'done') { $true } else { $null }
}
Check "the answered download completes" ([bool]$capDone) "never reached done"

$capAfter = (Invoke-Api "/api/captures" "GET" $true $null).body | ConvertFrom-Json
Check "nothing left waiting after answering" (@($capAfter.captures).Count -eq 0) "got $(@($capAfter.captures).Count)"

# The other answer. Clicking a link and changing one's mind must not leave a
# dialog behind that reopens on the next tick.
# Built by joining rather than by a literal: $originPort has to be expanded,
# and a single-quoted string would put the dollar sign into the URL, which the
# manager rightly refuses - the failure then surfaces several lines later as a
# 405 from a path with an empty id, which says nothing useful at all.
$cap2Body = '{"url":"http://127.0.0.1:' + $originPort + '/second.bin","filename":"second.bin"}'
$cap2     = Invoke-Api "/api/captures" "POST" $true $cap2Body
Check "a second capture accepted" ($cap2.code -eq 201) "got $($cap2.code): $($cap2.body)"
$cap2ID   = ''
try { $cap2ID = ($cap2.body | ConvertFrom-Json).capture.id } catch {}
Check "the second capture has an id" ([bool]$cap2ID) "no capture.id in $($cap2.body)"

$ignCap  = Invoke-Api "/api/captures/$cap2ID/ignore" "POST" $true $null
Check "not now puts it down" ($ignCap.code -eq 200) "got $($ignCap.code): $($ignCap.body)"
$capGone = (Invoke-Api "/api/captures" "GET" $true $null).body | ConvertFrom-Json
Check "and nothing left waiting" (@($capGone.captures).Count -eq 0) "still waiting"
$capGoneDl = (Invoke-Api "/api/downloads" "GET" $true $null).body | ConvertFrom-Json
Check "and nothing was started by it" (-not (@($capGoneDl.downloads) | Where-Object { $_.url -like '*second.bin' })) "a put-down capture still transferred"

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
# Every transfer by now has finished, and a transfer window whose job is over
# leaves the screen on its own - nobody closes it, the manager does. One left
# standing would be a window with no one inside to answer its buttons.
$leftover = Wait-For "transfer windows to go" 20000 { if (Get-StatusWindow) { $null } else { $true } }
Check "no transfer window outlives its transfer" ([bool]$leftover) "a GodlStatusWindow is still on screen after everything finished"

$guiProc.Refresh()
$mainHandle = $guiProc.MainWindowHandle
$guiProc.CloseMainWindow() | Out-Null
$trayHandle = Wait-For "tray icon window" 10000 {
    $h = [GodlStatusWin]::FindByClassForProcess("GodlTrayWindow", $guiProc.Id, $false)
    if ($h -ne [IntPtr]::Zero) { $h } else { $null }
}
$hidden = $null
if ($trayHandle) {
    $hidden = Wait-For "main window hidden to tray" 10000 {
        if ($mainHandle -ne [IntPtr]::Zero -and -not [GodlStatusWin]::IsWindowVisible($mainHandle)) { $true } else { $null }
    }
}
$guiProc.Refresh()
Check "tray icon registered" ([bool]$trayHandle) "no GodlTrayWindow for manager process"
Check "closing main window keeps manager alive in tray" ($trayHandle -and $hidden -and -not $guiProc.HasExited) "tray=$trayHandle hidden=$hidden main=$mainHandle visible=$([GodlStatusWin]::IsWindowVisible($mainHandle)) exited=$($guiProc.HasExited)"

if ($trayHandle -and $hidden) { [void][GodlStatusWin]::PostMessage($trayHandle, 0x8003, [IntPtr]::Zero, [IntPtr]::Zero) }
$reopened = Wait-For "tray Open restores main window" 10000 {
    if ($mainHandle -ne [IntPtr]::Zero -and [GodlStatusWin]::IsWindowVisible($mainHandle)) { $mainHandle } else { $null }
}
Check "tray Open restores main window" ([bool]$reopened) "main window did not return"

# A link handed over while the window is in the tray is the common case: the
# person is in the browser, and the browser is what owns the foreground. The
# window has to come back and come forward on its own - showing it underneath
# the browser is the same as not showing it.
$guiProc.CloseMainWindow() | Out-Null
$hiddenAgain = Wait-For "main window hidden again" 10000 {
    if ($mainHandle -ne [IntPtr]::Zero -and -not [GodlStatusWin]::IsWindowVisible($mainHandle)) { $true } else { $null }
}
Check "window hidden again before hand-off" ([bool]$hiddenAgain) "main window still visible"

$handURL = "http://127.0.0.1:$originPort/handoff.bin"
$handAdd = Invoke-Api "/api/captures" "POST" $true ('{"url":"' + $handURL + '","filename":"handoff.bin"}')
Check "hand-off accepted while in the tray" ($handAdd.code -eq 201) "got $($handAdd.code): $($handAdd.body)"
$handID = ''
try { $handID = ($handAdd.body | ConvertFrom-Json).capture.id } catch {}

$broughtBack = Wait-For "hand-off brings the window back" 10000 {
    if ($mainHandle -ne [IntPtr]::Zero -and [GodlStatusWin]::IsWindowVisible($mainHandle)) { $true } else { $null }
}
Check "hand-off brings the window back" ([bool]$broughtBack) "window stayed hidden after a browser hand-off"

$inFront = Wait-For "hand-off puts the window in front" 5000 {
    if ([GodlStatusWin]::GetForegroundWindow() -eq $mainHandle) { $true } else { $null }
}
Check "hand-off puts the window in front" ([bool]$inFront) "foreground=$([GodlStatusWin]::GetForegroundWindow()) manager=$mainHandle"

if ($handID) { [void](Invoke-Api "/api/captures/$handID/ignore" "POST" $true $null) }

# Closing to tray is not a quit; send the tray Exit action after reopen is proved.
$guiProc.Refresh()
Check "tray manager process remains alive" (-not $guiProc.HasExited) "process exited when main window closed"

if ($trayHandle) { [void][GodlStatusWin]::PostMessage($trayHandle, 0x8002, [IntPtr]::Zero, [IntPtr]::Zero) }
$exited = Wait-For "tray exit" 30000 { $guiProc.Refresh(); if ($guiProc.HasExited) { $true } else { $null } }
Check "tray Exit shuts manager down" ([bool]$exited) "still running after Exit command"

Check "endpoint removed on shutdown" (-not (Test-Path $endpointFile)) "api-endpoint still present"
Check "lock released on shutdown" (-not (Test-Path $lockFile)) "manager.lock still present"

# The check that only says something when the binary is built with -race, and
# costs nothing when it is not. A race the detector found is a corruption that
# happened during this very run, under real transfers - the half of the
# program that unit tests, however concurrent, never quite reach.
$stderr = if (Test-Path $guiLog) { Get-Content $guiLog -Raw } else { '' }
$racy = $stderr -match 'WARNING: DATA RACE'
$evidence = ''
if ($racy) { $evidence = (($stderr -split "`r?`n") | Select-Object -First 8) -join ' // ' }
Check "manager reported no data race" (-not $racy) $evidence

# The regression test for the bug that took an afternoon to find. A run under
# a scratch GODL_HOME must leave the machine's browser tables exactly as it
# found them: they used to be rewritten on every launch, which quietly
# unhooked a working install from its own browser. Read here, before Cleanup
# puts back whatever was saved at the top - so a manager that still writes
# them fails this check rather than being silently repaired by the test.
$untouched = $true
$diff = ''
foreach ($k in $script:browserKeys) {
    $now = (Get-ItemProperty -Path $k -ErrorAction SilentlyContinue).'(default)'
    $before = $null
    if ($script:savedKeys.ContainsKey($k)) { $before = $script:savedKeys[$k] }
    if ($now -ne $before) { $untouched = $false; $diff = "$k : '$before' -> '$now'" }
}
Check "scratch run left the machine's browser tables alone" $untouched $diff

# ── summary ────────────────────────────────────────────────────────────────
Cleanup

Write-Output ""
Write-Output ("RESULT  {0} passed, {1} failed" -f $script:pass, $script:fail)
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
