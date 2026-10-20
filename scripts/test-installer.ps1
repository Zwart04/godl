$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$work = Join-Path $env:TEMP ("godl-inst-" + (Get-Random))
$dir  = Join-Path $work "godl"
$save = Join-Path $work "save"
$setup = Join-Path $root "godl-setup.exe"
$desktop = [Environment]::GetFolderPath('Desktop')
$menu    = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
$runPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"

# The browser keys, named as the assertions below name them.
$keys = @(
  @{ Label = "chrome";  Path = "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.godl.host" },
  @{ Label = "edge";    Path = "HKCU:\Software\Microsoft\Edge\NativeMessagingHosts\com.godl.host" },
  @{ Label = "firefox"; Path = "HKCU:\Software\Mozilla\NativeMessagingHosts\com.godl.moz" }
)

$pass = 0; $fail = 0
function Check($name, $cond, $detail) {
  if ($cond) { Write-Output "  PASS  $name"; $script:pass++ }
  else       { Write-Output "  FAIL  $name  ($detail)"; $script:fail++ }
}

# ── a home of its own ───────────────────────────────────────────────────
#
# Records, the single-instance lock and the browser manifest all live under
# GODL_HOME. Without one, this run would take the lock of - and write over -
# whatever installation the machine already has, and the copy being installed
# could not start at all while another instance held it. With one, the two
# coexist and the existing history is never read, written or locked.
$oldHome  = $env:GODL_HOME
$godlHome = Join-Path $work "home"
$env:GODL_HOME = $godlHome
$manifestPath  = Join-Path $godlHome "com.godl.host.json"

# ── machine-wide state this run is about to disturb ─────────────────────
#
# The shortcut names, the Run value and the browser keys belong to the
# machine, not to the folder being installed into: an install points them at
# this test's copy, and the uninstall then removes them. That would orphan a
# real installation - its binaries survive while every pointer to them does
# not - and one test run would cost somebody their app. So whatever is there
# now is saved first and put back once the assertions have had their say.
$hadMenu    = Test-Path (Join-Path $menu "godl.lnk")
$hadDesktop = Test-Path (Join-Path $desktop "godl.lnk")
$hadRun     = $null -ne (Get-ItemProperty -Path $runPath -Name godl -ErrorAction SilentlyContinue)
$savedRun   = (Get-ItemProperty -Path $runPath -ErrorAction SilentlyContinue).godl
$savedKeys  = @{}
foreach ($k in $keys) {
  $v = Get-ItemProperty -Path $k.Path -ErrorAction SilentlyContinue
  $savedKeys[$k.Path] = if ($null -eq $v) { $null } else { [string]$v.'(default)' }
}
New-Item -ItemType Directory -Path $save -Force | Out-Null
if ($hadMenu)    { Copy-Item (Join-Path $menu "godl.lnk") (Join-Path $save "menu.lnk") -Force }
if ($hadDesktop) { Copy-Item (Join-Path $desktop "godl.lnk") (Join-Path $save "desktop.lnk") -Force }

# Puts back what was found. It says nothing about how the run itself went -
# the assertions above have already reported that - because its job is to
# leave the machine as it was whether the run passed, failed or stopped early.
function Restore-Previous {
  $ErrorActionPreference = 'SilentlyContinue'
  if ($hadMenu)    { Copy-Item (Join-Path $save "menu.lnk") (Join-Path $menu "godl.lnk") -Force }
  if ($hadDesktop) { Copy-Item (Join-Path $save "desktop.lnk") (Join-Path $desktop "godl.lnk") -Force }
  if ($hadRun)     { Set-ItemProperty -Path $runPath -Name godl -Value $savedRun }
  foreach ($k in $keys) {
    $want = $savedKeys[$k.Path]
    # Absent before is exactly where the uninstall left it, and that is right.
    if ($null -eq $want) { continue }
    New-Item -Path $k.Path -Force | Out-Null
    New-ItemProperty -Path $k.Path -Name '(default)' -Value $want -PropertyType String -Force | Out-Null
  }
}

function Finish {
  Restore-Previous
  $env:GODL_HOME = $oldHome
}

# A terminating error skips every line after the one that raised it, including
# the restoration at the bottom. The trap closes that gap.
trap {
  Finish
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
  Write-Output ""
  Write-Output "ABORT: $($_.Exception.Message)"
  exit 1
}

# The processes this test owns. Filtering by path matters: every godl.exe on
# the machine answers to the same name, and killing all of them would stop
# somebody's transfers to find out whether an installer had worked.
function Owned-Process {
  @(Get-Process godl,godl-host -ErrorAction SilentlyContinue |
      Where-Object { $_.Path -and $_.Path.StartsWith($dir, [StringComparison]::OrdinalIgnoreCase) })
}

Write-Output "=== install (default options) ==="
# Not -Wait: that waits for the process *tree*, and the installer deliberately
# leaves godl.exe running behind it. Poll for the installer's own exit instead.
$proc = Start-Process -FilePath $setup -ArgumentList @("-dir=`"$dir`"", "-quiet") -PassThru
$deadline = (Get-Date).AddSeconds(90)
while (-not $proc.HasExited -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }
$proc.Refresh()
Check "installer exited on its own" $proc.HasExited "still running after 90s"
Check "installer reported success"  ($proc.ExitCode -eq 0) "exit code $($proc.ExitCode)"
Start-Sleep -Seconds 3

Check "godl.exe extracted"        (Test-Path "$dir\godl.exe")        "missing"
Check "godl-host.exe extracted"   (Test-Path "$dir\godl-host.exe")   "missing"
Check "godl.exe size plausible"   ((Get-Item "$dir\godl.exe").Length -gt 10MB) "too small"
Check "host size plausible"       ((Get-Item "$dir\godl-host.exe").Length -gt 8MB) "too small"
Check "no leftover .new files"    (@(Get-ChildItem $dir -Filter *.new -ErrorAction SilentlyContinue).Count -eq 0) "found .new"

# The extension ships with the programs so "Load unpacked" has a folder to
# point at on a machine that has never seen this repository.
$extManifest = Join-Path $dir "extension\chrome\manifest.json"
Check "browser extension extracted" (Test-Path $extManifest) "missing $extManifest"
Check "firefox extension extracted" (Test-Path (Join-Path $dir "extension\firefox\manifest.json")) "missing firefox manifest"
Check "extension icons extracted"   (Test-Path (Join-Path $dir "extension\chrome\icons\128.png")) "missing extension icon"
if (Test-Path $extManifest) {
  $ext = Get-Content $extManifest -Raw | ConvertFrom-Json
  Check "extension asks for notifications" ($ext.permissions -contains "notifications") "notifications permission missing"
}

Check "start menu shortcut" (Test-Path (Join-Path $menu "godl.lnk")) "missing"
Check "desktop shortcut"    (Test-Path (Join-Path $desktop "godl.lnk")) "missing"

$run = (Get-ItemProperty -Path $runPath -ErrorAction SilentlyContinue).godl
Check "autostart registered" ($run -eq "`"$dir\godl.exe`"") "got '$run'"

$reg = (Get-ItemProperty -Path $keys[0].Path -ErrorAction SilentlyContinue).'(default)'
Check "chrome registry key" ($reg -eq $manifestPath) "got '$reg'"

$mf = Get-Content $manifestPath -Raw | ConvertFrom-Json
Check "manifest points at installed host" ($mf.path -eq "$dir\godl-host.exe") "got '$($mf.path)'"
Check "manifest trusts our extension"     ($mf.allowed_origins -contains "chrome-extension://afamnbklhodbngmbljglpkklkidfjccn/") "got '$($mf.allowed_origins -join ',')'"

Check "godl launched" (@(Owned-Process | Where-Object Name -eq 'godl').Count -gt 0) "not running"

Write-Output ""
Write-Output "=== uninstall ==="
# Only this test's own. A godl running from anywhere else is somebody's
# window, and the uninstall removes nothing of its anyway.
@(Owned-Process) | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
$proc = Start-Process -FilePath $setup -ArgumentList @("-dir=`"$dir`"", "-uninstall", "-quiet") -PassThru
$deadline = (Get-Date).AddSeconds(60)
while (-not $proc.HasExited -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }
$proc.Refresh()
Check "uninstaller exited" $proc.HasExited "still running"
Check "uninstaller reported success" ($proc.ExitCode -eq 0) "exit code $($proc.ExitCode)"
Start-Sleep -Seconds 2

Check "install folder removed" (-not (Test-Path $dir)) "still present"
Check "start menu shortcut gone" (-not (Test-Path (Join-Path $menu "godl.lnk"))) "still present"
Check "desktop shortcut gone"    (-not (Test-Path (Join-Path $desktop "godl.lnk"))) "still present"
Check "autostart removed" (-not $run -or $null -eq (Get-ItemProperty -Path $runPath -Name godl -ErrorAction SilentlyContinue)) "still present"
foreach ($k in $keys) {
  Check "$($k.Label) key removed" ($null -eq (Get-ItemProperty -Path $k.Path -ErrorAction SilentlyContinue)) "still present"
}
Check "manifest removed"   (-not (Test-Path $manifestPath)) "still present"

Write-Output ""
Write-Output "=== what was here before is put back ==="
Finish

# Only worth asserting when there was something to begin with. On a clean
# machine these would be checks that nothing exists, which the run above has
# already shown for every one of them.
if ($hadMenu)    { Check "the start menu shortcut is back" (Test-Path (Join-Path $menu "godl.lnk")) "not restored" }
if ($hadDesktop) { Check "the desktop shortcut is back"    (Test-Path (Join-Path $desktop "godl.lnk")) "not restored" }
if ($hadRun)     { Check "the autostart entry is back"     ((Get-ItemProperty -Path $runPath -ErrorAction SilentlyContinue).godl -eq $savedRun) "not restored" }
foreach ($k in $keys) {
  $want = $savedKeys[$k.Path]
  if ($null -eq $want) { continue }
  $now = (Get-ItemProperty -Path $k.Path -ErrorAction SilentlyContinue).'(default)'
  Check "the $($k.Label) key is back" ($now -eq $want) "got '$now', want '$want'"
}

Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
Write-Output ""
Write-Output "RESULT  $pass passed, $fail failed"
# The success path has to exit explicitly. Falling off the end leaves
# $LASTEXITCODE at whatever the last native command happened to return, so a
# caller checking it would see a number that says nothing about this run.
if ($fail -gt 0) { exit 1 } else { exit 0 }
