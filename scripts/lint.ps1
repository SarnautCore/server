$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repositoryRoot
try {
    go vet ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go vet failed with exit code $LASTEXITCODE"
    }

    go tool '-modfile=golangci-lint.mod' github.com/golangci/golangci-lint/v2/cmd/golangci-lint run
    if ($LASTEXITCODE -ne 0) {
        throw "golangci-lint failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
