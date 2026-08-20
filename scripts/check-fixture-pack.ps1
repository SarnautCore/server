# Fails if any content pack committed to this repository carries the untyped
# `extra:` passthrough. Those rows hold verbatim MY.GAMES type and attribute
# names, and a public repository must never ship them (ADR 0011, ADR 0029).

$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$packsRoot = Join-Path $repositoryRoot "testdata\packs"

if (-not (Test-Path -LiteralPath $packsRoot -PathType Container)) {
    Write-Output "No vendored packs found at $packsRoot."
    exit 0
}

$manifests = Get-ChildItem -LiteralPath $packsRoot -Recurse -Filter "manifest.json" -File
if ($manifests.Count -eq 0) {
    throw "No pack manifest found under $packsRoot. The fixture pack every test shares is missing."
}

$failed = $false
foreach ($manifest in $manifests) {
    $document = Get-Content -LiteralPath $manifest.FullName -Raw | ConvertFrom-Json
    if ($document.keep_extra) {
        Write-Error "$($manifest.FullName) records keep_extra: true"
        $failed = $true
        continue
    }
    Write-Output "$($manifest.FullName): pack_id $($document.pack_id), keep_extra false"
}

if ($failed) {
    throw "A committed pack carries the untyped extra passthrough."
}
