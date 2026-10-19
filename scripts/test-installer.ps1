$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$work = Join-Path $env:TEMP ("godl-inst-" + (Get-Random))
$dir  = Join-Path $work "godl"
$setup = Join-Path $root "godl-setup.exe"
$desktop = [Environment]::GetFolderPath('Desktop')
$menu    = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"

$pass = 0; $fail = 0
function Check($name, $cond, $detail) {
  if ($cond) { Write-Output "  PASS  $name"; $script:pass++ }
  else       { Write-Output "  FAIL  $name  ($detail)"; $script:fail++ }
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

Check "start menu shortcut" (Test-Path (Join-Path $menu "godl.lnk")) "missing"
Check "desktop shortcut"    (Test-Path (Join-Path $desktop "godl.lnk")) "missing"

$run = (Get-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -ErrorAction SilentlyContinue).godl
Check "autostart registered" ($run -eq "`"$dir\godl.exe`"") "got '$run'"

$manifestPath = Join-Path $env:APPDATA "godl\com.godl.host.json"
$reg = (Get-ItemProperty -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.godl.host" -ErrorAction SilentlyContinue).'(default)'
Check "chrome registry key" ($reg -eq $manifestPath) "got '$reg'"

$mf = Get-Content $manifestPath -Raw | ConvertFrom-Json
Check "manifest points at installed host" ($mf.path -eq "$dir\godl-host.exe") "got '$($mf.path)'"
Check "manifest trusts our extension"     ($mf.allowed_origins -contains "chrome-extension://afamnbklhodbngmbljglpkklkidfjccn/") "got '$($mf.allowed_origins -join ',')'"

Check "godl launched" (@(Get-Process godl -ErrorAction SilentlyContinue).Count -gt 0) "not running"

Write-Output ""
Write-Output "=== uninstall ==="
Get-Process godl,godl-host -ErrorAction SilentlyContinue | Stop-Process -Force
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
Check "autostart removed" (-not $run -or $null -eq (Get-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name godl -ErrorAction SilentlyContinue)) "still present"
Check "chrome key removed" ($null -eq (Get-ItemProperty -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.godl.host" -ErrorAction SilentlyContinue)) "still present"
Check "edge key removed"   ($null -eq (Get-ItemProperty -Path "HKCU:\Software\Microsoft\Edge\NativeMessagingHosts\com.godl.host" -ErrorAction SilentlyContinue)) "still present"
Check "firefox key removed"($null -eq (Get-ItemProperty -Path "HKCU:\Software\Mozilla\NativeMessagingHosts\com.godl.moz" -ErrorAction SilentlyContinue)) "still present"
Check "manifest removed"   (-not (Test-Path $manifestPath)) "still present"

Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
Write-Output ""
Write-Output "RESULT  $pass passed, $fail failed"
# The success path has to exit explicitly. Falling off the end leaves
# $LASTEXITCODE at whatever the last native command happened to return, so a
# caller checking it would see a number that says nothing about this run.
if ($fail -gt 0) { exit 1 } else { exit 0 }
