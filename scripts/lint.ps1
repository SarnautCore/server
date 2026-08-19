$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repositoryRoot
try {
    go vet ./...
    golangci-lint run
}
finally {
    Pop-Location
}
