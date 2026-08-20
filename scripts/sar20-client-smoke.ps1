param(
    [string]$ClientRepository = (Join-Path $PSScriptRoot "..\..\client"),
    [string]$Address = "127.0.0.1:4342",
    [string]$HealthAddress = "127.0.0.1:8181"
)

$ErrorActionPreference = "Stop"
$serverRepository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$clientRepositoryPath = (Resolve-Path $ClientRepository).Path
$smokeProject = Join-Path $clientRepositoryPath "tools\SarnautCore.NetSmoke\SarnautCore.NetSmoke.csproj"
if (-not (Test-Path -LiteralPath $smokeProject -PathType Leaf)) {
    throw "SAR-20 smoke client was not found at $smokeProject"
}

# The smoke is the only place the two repositories meet at runtime. There is no
# transition period in which both framings are accepted (ADR 0026), so a client
# whose proto tree has drifted from this server would mis-parse rather than
# fail, which is exactly the failure the envelope exists to prevent.
$syncScript = Join-Path $clientRepositoryPath "scripts\sync-proto.ps1"
if (-not (Test-Path -LiteralPath $syncScript -PathType Leaf)) {
    throw "The client repository at $clientRepositoryPath has no scripts/sync-proto.ps1 (ADR 0027)."
}
& $syncScript -ServerRepo $serverRepository -Check
if ($LASTEXITCODE -ne 0) {
    throw "The client proto tree differs from $serverRepository. Run client/scripts/sync-proto.ps1."
}

$goCommand = Get-Command go -ErrorAction SilentlyContinue
$goExecutable = if ($null -ne $goCommand) { $goCommand.Source } else { $null }
if ($null -eq $goCommand -and $IsWindows) {
    $windowsGo = "C:\Program Files\Go\bin\go.exe"
    if (Test-Path -LiteralPath $windowsGo -PathType Leaf) {
        $goExecutable = $windowsGo
    }
}
if ($null -eq $goExecutable) {
    throw "Go was not found on PATH. Install the version from go.mod."
}

$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("sarnaut-sar20-" + [Guid]::NewGuid().ToString("N"))
$contentRoot = Join-Path $temporaryRoot "content"
$tableDirectory = Join-Path $contentRoot "classic\zones\smoke-zone\spawns\tables"
$placementDirectory = Join-Path $contentRoot "classic\zones\smoke-zone\spawns\placements"
$binaryExtension = if ($IsWindows) { ".exe" } else { "" }
$shardBinary = Join-Path $temporaryRoot ("shard-smoke" + $binaryExtension)
$stdoutPath = Join-Path $temporaryRoot "shard.stdout.log"
$stderrPath = Join-Path $temporaryRoot "shard.stderr.log"
$serverProcess = $null

$environment = @{
    SARNAUT_QUIC_LISTEN_ADDRESS = $Address
    SARNAUT_HEALTH_ADDRESS = $HealthAddress
    SARNAUT_CONTENT_ROOT = $contentRoot
    SARNAUT_CONTENT_RULESET = "classic"
    SARNAUT_CONTENT_ZONE_SLUG = "smoke-zone"
    SARNAUT_WORLD_ZONE_ID = "InstLeague1"
    SARNAUT_NATS_URL = ""
    SARNAUT_POSTGRES_DSN = ""
    SARNAUT_VALKEY_ADDRESS = ""
    SARNAUT_OTEL_ENDPOINT = ""
}
$previousEnvironment = @{}

try {
    New-Item -ItemType Directory -Path $tableDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $placementDirectory -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $tableDirectory "empty.yaml") -Encoding utf8 -Value @"
id: spawn.smoke.empty
entries: []
"@
    Set-Content -LiteralPath (Join-Path $placementDirectory "empty.yaml") -Encoding utf8 -Value @"
placements: []
"@

    & $goExecutable build -o $shardBinary ./cmd/shard
    if ($LASTEXITCODE -ne 0) {
        throw "Building the shard failed with exit code $LASTEXITCODE."
    }

    foreach ($name in $environment.Keys) {
        $previousEnvironment[$name] = [System.Environment]::GetEnvironmentVariable($name, "Process")
        [System.Environment]::SetEnvironmentVariable($name, $environment[$name], "Process")
    }

    $startArguments = @{
        FilePath = $shardBinary
        WorkingDirectory = $serverRepository
        RedirectStandardOutput = $stdoutPath
        RedirectStandardError = $stderrPath
        PassThru = $true
    }
    if ($IsWindows) {
        $startArguments.WindowStyle = "Hidden"
    }
    $serverProcess = Start-Process @startArguments

    $readyUri = "http://$HealthAddress/readyz"
    $ready = $false
    for ($attempt = 0; $attempt -lt 80; $attempt++) {
        if ($serverProcess.HasExited) {
            $serverError = Get-Content -LiteralPath $stderrPath -Raw -ErrorAction SilentlyContinue
            throw "The shard exited before readiness. $serverError"
        }

        try {
            $response = Invoke-WebRequest -Uri $readyUri -UseBasicParsing -TimeoutSec 1
            if ($response.StatusCode -eq 200) {
                $ready = $true
                break
            }
        }
        catch {
        }
        Start-Sleep -Milliseconds 250
    }
    if (-not $ready) {
        throw "The shard did not become ready at $readyUri within 20 seconds."
    }

    & dotnet run --project $smokeProject --configuration Debug -- --address $Address --zone InstLeague1 --duration 8
    if ($LASTEXITCODE -ne 0) {
        throw "The SAR-20 client smoke failed with exit code $LASTEXITCODE."
    }
}
finally {
    if ($null -ne $serverProcess -and -not $serverProcess.HasExited) {
        Stop-Process -Id $serverProcess.Id
        Wait-Process -Id $serverProcess.Id -Timeout 5 -ErrorAction SilentlyContinue
    }

    foreach ($name in $environment.Keys) {
        [System.Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], "Process")
    }

    if (Test-Path -LiteralPath $temporaryRoot) {
        $resolvedTemporaryRoot = [System.IO.Path]::GetFullPath($temporaryRoot)
        $safeTemporaryParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
        if ($resolvedTemporaryRoot.StartsWith($safeTemporaryParent, [StringComparison]::OrdinalIgnoreCase) -and
            $resolvedTemporaryRoot -ne $safeTemporaryParent) {
            Remove-Item -LiteralPath $resolvedTemporaryRoot -Recurse -Force
        }
    }
}
