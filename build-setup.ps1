# Builds godl-setup.exe, the single file a person actually downloads.
#
# godl is two programs - godl.exe for them, godl-host.exe for their browser -
# and the whole point of this script is that nobody outside this repository
# ever has to know that. Both are packed into one archive, embedded into one
# windowed executable, and unpacked by the installer into place.
#
#   .\build-setup.ps1
#
# Result: godl-setup.exe in the project root.

$ErrorActionPreference = 'Stop'

$root   = Split-Path -Parent $MyInvocation.MyCommand.Path
$goExe  = Join-Path (& go env GOROOT) 'bin\go.exe'
$payDir = Join-Path $root 'cmd\godl-setup\payload'
$outDir = Join-Path $root 'build\bin'

if (-not (Test-Path $payDir)) { New-Item -ItemType Directory -Force -Path $payDir | Out-Null }
if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }

Push-Location $root
try {
  # 1. The two programs the installer will carry.
  Write-Host 'building godl.exe'
  & (Join-Path $root 'bin\wails.exe') build | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'wails build failed' }

  Write-Host 'building godl-host.exe'
  & $goExe build -o (Join-Path $outDir 'godl-host.exe') .\cmd\godl-host
  if ($LASTEXITCODE -ne 0) { throw 'godl-host build failed' }

  # 2. One archive per program.
  #
  # Zip rather than bare gzip: the central directory carries the exact
  # uncompressed size, which lets the installer refuse a truncated payload
  # instead of writing an executable that Windows will not start.
  foreach ($name in @('godl.exe', 'godl-host.exe')) {
    $src = Join-Path $outDir $name
    if (-not (Test-Path $src)) { throw "$src is missing" }

    $zip = Join-Path $payDir ($name + '.zip')
    if (Test-Path $zip) { Remove-Item -Force $zip }

    # Compress-Archive is given a file, not a folder, so the entry lands at
    # the archive's root - which is where unpack() looks for it.
    Compress-Archive -Path $src -DestinationPath $zip -CompressionLevel Optimal

    $ratio = [math]::Round((Get-Item $zip).Length / (Get-Item $src).Length * 100)
    Write-Host ('  {0,-16} {1,6:N0} KB  ({2}% of the original)' -f $name, `
      ((Get-Item $zip).Length / 1KB), $ratio)
  }

  # 3. The installer itself.
  #
  # -H windowsgui keeps it out of the way in a terminal: there is no console
  # window and no output to read, only the message box at the end.
  Write-Host 'building godl-setup.exe'
  & $goExe build -tags setup -trimpath `
    -ldflags='-s -w -H windowsgui' `
    -o (Join-Path $outDir 'godl-setup.exe') .\cmd\godl-setup
  if ($LASTEXITCODE -ne 0) { throw 'godl-setup build failed' }

  $setup = Join-Path $outDir 'godl-setup.exe'
  Copy-Item -Force $setup (Join-Path $root 'godl-setup.exe')

  $total = [math]::Round(((Get-Item $outDir).Length))
  Write-Host ''
  Write-Host ('godl-setup.exe  {0:N1} MB' -f ((Get-Item $setup).Length / 1MB))
  Write-Host ('  carries       {0:N1} MB of programs' -f `
    (((Get-Item (Join-Path $outDir 'godl.exe')).Length + (Get-Item (Join-Path $outDir 'godl-host.exe')).Length) / 1MB))
}
finally {
  Pop-Location
}
