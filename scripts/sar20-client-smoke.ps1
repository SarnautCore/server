param(
    [string]$ClientRepository = (Join-Path $PSScriptRoot "..\..\client"),
    [string]$Address = "127.0.0.1:4342",
    [string]$HealthAddress = "127.0.0.1:8181",
    [string]$AuthAddress = "127.0.0.1:8183",
    [string]$AuthHealthAddress = "127.0.0.1:8182",
    [string]$PostgresDsn = "postgres://sarnaut:sarnaut_dev@127.0.0.1:5433/sarnaut?sslmode=disable",
    [string]$ValkeyAddress = "127.0.0.1:6379",
    [string]$NatsUrl = "nats://127.0.0.1:4222"
)

# The shard admits nobody without an ADR 0030 ticket, so this smoke now boots the
# auth service too and performs the out-of-band flow the launcher will perform:
# register, log in, create a character, mint a ticket, and hand it to the .NET
# client. The client itself gains no login UI in this wave; that gap is accepted
# and closes in the next one.
#
# PostgreSQL, Valkey and NATS are preconditions rather than something this script
# starts. Auth and the shard refuse to run without them:
#
#     docker compose -f ..\infra\compose\docker-compose.yml up -d

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
try {
    & $syncScript -ServerRepo $serverRepository -Check
}
catch {
    throw "The client proto tree differs from $serverRepository. Run client/scripts/sync-proto.ps1. $_"
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

# A shard left running on these ports answers /readyz and accepts the
# connection, so the smoke would report on someone else's binary — and on the
# wire format that binary was built with. Refuse instead of guessing.
$quicPort = [int]($Address -split ":")[-1]
$healthPort = [int]($HealthAddress -split ":")[-1]
$authPort = [int]($AuthAddress -split ":")[-1]
$authHealthPort = [int]($AuthHealthAddress -split ":")[-1]
foreach ($occupied in @(
    (Get-NetUDPEndpoint -LocalPort $quicPort -ErrorAction SilentlyContinue),
    (Get-NetTCPConnection -LocalPort $healthPort -State Listen -ErrorAction SilentlyContinue),
    (Get-NetTCPConnection -LocalPort $authPort -State Listen -ErrorAction SilentlyContinue),
    (Get-NetTCPConnection -LocalPort $authHealthPort -State Listen -ErrorAction SilentlyContinue)
)) {
    if ($null -ne $occupied) {
        $owner = ($occupied | Select-Object -First 1).OwningProcess
        throw "Port $quicPort or $healthPort is already held by process $owner. Stop it, or pass -Address and -HealthAddress."
    }
}

$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("sarnaut-sar20-" + [Guid]::NewGuid().ToString("N"))
# The vendored golden fixture pack: invented content, no private data repository
# and no YAML at runtime (ADR 0029).
$contentPack = Join-Path $serverRepository "testdata\packs\demo"
$binaryExtension = if ($IsWindows) { ".exe" } else { "" }
$shardBinary = Join-Path $temporaryRoot ("shard-smoke" + $binaryExtension)
$authBinary = Join-Path $temporaryRoot ("auth-smoke" + $binaryExtension)
$migrateBinary = Join-Path $temporaryRoot ("migrate-smoke" + $binaryExtension)
$stdoutPath = Join-Path $temporaryRoot "shard.stdout.log"
$stderrPath = Join-Path $temporaryRoot "shard.stderr.log"
$authStdoutPath = Join-Path $temporaryRoot "auth.stdout.log"
$authStderrPath = Join-Path $temporaryRoot "auth.stderr.log"
$serverProcess = $null
$authProcess = $null

$environment = @{
    SARNAUT_QUIC_LISTEN_ADDRESS = $Address
    SARNAUT_HEALTH_ADDRESS = $HealthAddress
    SARNAUT_CONTENT_PACK = $contentPack
    SARNAUT_WORLD_ZONE_ID = "InstLeague1"
    SARNAUT_NATS_URL = $NatsUrl
    SARNAUT_POSTGRES_DSN = $PostgresDsn
    SARNAUT_VALKEY_ADDRESS = $ValkeyAddress
    SARNAUT_AUTH_LISTEN_ADDRESS = $AuthAddress
    SARNAUT_OTEL_ENDPOINT = ""
}
$previousEnvironment = @{}

try {
    New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
    if (-not (Test-Path -LiteralPath (Join-Path $contentPack "manifest.json") -PathType Leaf)) {
        throw "The vendored fixture pack was not found at $contentPack."
    }

    & $goExecutable build -o $shardBinary ./cmd/shard
    if ($LASTEXITCODE -ne 0) {
        throw "Building the shard failed with exit code $LASTEXITCODE."
    }
    & $goExecutable build -o $authBinary ./cmd/auth
    if ($LASTEXITCODE -ne 0) {
        throw "Building auth failed with exit code $LASTEXITCODE."
    }
    & $goExecutable build -o $migrateBinary ./cmd/migrate
    if ($LASTEXITCODE -ne 0) {
        throw "Building the migrator failed with exit code $LASTEXITCODE."
    }

    foreach ($name in $environment.Keys) {
        $previousEnvironment[$name] = [System.Environment]::GetEnvironmentVariable($name, "Process")
        [System.Environment]::SetEnvironmentVariable($name, $environment[$name], "Process")
    }

    & $migrateBinary up
    if ($LASTEXITCODE -ne 0) {
        throw "Migrations failed. Is infra/compose running? docker compose -f ..\infra\compose\docker-compose.yml up -d"
    }

    $authArguments = @{
        FilePath = $authBinary
        WorkingDirectory = $serverRepository
        RedirectStandardOutput = $authStdoutPath
        RedirectStandardError = $authStderrPath
        PassThru = $true
    }
    if ($IsWindows) {
        $authArguments.WindowStyle = "Hidden"
    }
    # Auth and the shard share every variable except the health address, so only
    # that one is swapped around the start.
    [System.Environment]::SetEnvironmentVariable("SARNAUT_HEALTH_ADDRESS", $AuthHealthAddress, "Process")
    $authProcess = Start-Process @authArguments
    [System.Environment]::SetEnvironmentVariable("SARNAUT_HEALTH_ADDRESS", $HealthAddress, "Process")

    $authReadyUri = "http://$AuthHealthAddress/readyz"
    $authReady = $false
    for ($attempt = 0; $attempt -lt 80; $attempt++) {
        if ($authProcess.HasExited) {
            $authError = Get-Content -LiteralPath $authStderrPath -Raw -ErrorAction SilentlyContinue
            throw "Auth exited before readiness. $authError"
        }
        try {
            if ((Invoke-WebRequest -Uri $authReadyUri -UseBasicParsing -TimeoutSec 1).StatusCode -eq 200) {
                $authReady = $true
                break
            }
        }
        catch {
        }
        Start-Sleep -Milliseconds 250
    }
    if (-not $authReady) {
        throw "Auth did not become ready at $authReadyUri within 20 seconds."
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

    # The out-of-band flow of protocol/session.md rule 5.3, spelled out against
    # the HTTP API so this script doubles as its worked example.
    $runId = -join ((1..6) | ForEach-Object { [char](97 + (Get-Random -Maximum 26)) })
    $credentials = @{ email = "sar20-$runId@example.invalid"; password = "sar20-password-$runId" }
    Invoke-RestMethod -Method Post -Uri "http://$AuthAddress/v1/accounts" `
        -ContentType "application/json" -Body ($credentials | ConvertTo-Json) -TimeoutSec 5 | Out-Null
    $session = Invoke-RestMethod -Method Post -Uri "http://$AuthAddress/v1/sessions" `
        -ContentType "application/json" -Body ($credentials | ConvertTo-Json) -TimeoutSec 5
    $authHeader = @{ Authorization = "Bearer $($session.session_token)" }
    $options = Invoke-RestMethod -Uri "http://$AuthAddress/v1/chargen/options" -TimeoutSec 5
    if ($options.options.Count -lt 1) {
        throw "The auth service offers no chargen options; the pack carries no chargen table (ADR 0032)."
    }
    $character = Invoke-RestMethod -Method Post -Uri "http://$AuthAddress/v1/characters" `
        -Headers $authHeader -ContentType "application/json" `
        -Body (@{ name = "Smoke$runId"; chargen_option_id = $options.options[0].id } | ConvertTo-Json) -TimeoutSec 5
    $ticket = Invoke-RestMethod -Method Post -Uri "http://$AuthAddress/v1/tickets" `
        -Headers $authHeader -ContentType "application/json" `
        -Body (@{ character_id = $character.character_id } | ConvertTo-Json) -TimeoutSec 5
    Write-Output "minted a shard ticket for character $($character.name) ($($character.character_id))"

    & dotnet run --project $smokeProject --configuration Debug -- `
        --address $Address --zone InstLeague1 --duration 8 --ticket $ticket.ticket
    if ($LASTEXITCODE -ne 0) {
        throw "The SAR-20 client smoke failed with exit code $LASTEXITCODE."
    }
}
finally {
    foreach ($process in @($serverProcess, $authProcess)) {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Wait-Process -Id $process.Id -Timeout 5 -ErrorAction SilentlyContinue
        }
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
