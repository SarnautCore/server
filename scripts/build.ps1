$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repositoryRoot
try {
    New-Item -ItemType Directory -Force -Path "bin" | Out-Null
    go build -o "bin/shard.exe" ./cmd/shard
    go build -o "bin/auth.exe" ./cmd/auth
    go build -o "bin/gateway.exe" ./cmd/gateway
}
finally {
    Pop-Location
}
