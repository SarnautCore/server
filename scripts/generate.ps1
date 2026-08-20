$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repositoryRoot
try {
    go generate ./proto
    if ($LASTEXITCODE -ne 0) {
        throw "protobuf generation failed with exit code $LASTEXITCODE."
    }
    & (Join-Path $PSScriptRoot "proto-lock.ps1")
}
finally {
    Pop-Location
}
