param(
    [Parameter(Mandatory)][string]$PreviousBinary,
    [Parameter(Mandatory)][string]$MySQLBin,
    [Parameter(Mandatory)][int]$SQLPort,
    [Parameter(Mandatory)][string]$Fixture
)

$ErrorActionPreference = 'Stop'
$current = Join-Path $Fixture 'agent-memory-current.exe'
go build -trimpath -o $current ./cmd/agent-memory
if ($LASTEXITCODE) { throw 'Could not build the current application.' }
$old = (Resolve-Path -LiteralPath $PreviousBinary).Path
$mysql = Join-Path $MySQLBin 'mysql.exe'
$dumpTool = Join-Path $MySQLBin 'mysqldump.exe'
$dump = Join-Path $Fixture 'before.sql'
$connection = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$SQLPort", '--user=memory_test')
$saved = @{}
foreach ($name in @('MEMORY_MODEL_URL','MEMORY_KEY_FILE','MEMORY_JOURNAL','MEMORY_MYSQL_DSN')) {
    $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$process = $null
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
try { $port = $listener.LocalEndpoint.Port } finally { $listener.Stop() }
$address = "http://127.0.0.1:$port"

function Invoke-Binary($Binary, [string[]]$Arguments) {
    $result = & $Binary @Arguments
    if ($LASTEXITCODE) { throw "Application command '$($Arguments[0])' failed." }
    $result
}
function Start-TestServer($Binary) {
    $started = Start-Process -FilePath $Binary -ArgumentList @('serve', '-listen', "127.0.0.1:$port") -WindowStyle Hidden -PassThru
    for ($i = 0; $i -lt 40 -and !$started.HasExited; $i++) {
        try {
            if ((Invoke-WebRequest -TimeoutSec 1 -Uri "$address/healthz").StatusCode -eq 200) { return $started }
        } catch {}
        Start-Sleep -Milliseconds 125
    }
    if (!$started.HasExited) { Stop-Process -InputObject $started; $started.WaitForExit() }
    throw 'The application did not become healthy.'
}
function Invoke-API($Method, $Path, $Token, $Payload, [int]$Status = 200) {
    $options = @{Method=$Method; Uri="$address/api/$Path"; Headers=@{Authorization="Bearer $Token"};
        SkipHttpErrorCheck=$true; TimeoutSec=8}
    if ($null -ne $Payload) {
        $options.ContentType = 'application/json'
        $options.Body = $Payload | ConvertTo-Json -Depth 6 -Compress
    }
    $response = Invoke-WebRequest @options
    if ($response.StatusCode -ne $Status) { throw "$Method $Path returned $($response.StatusCode), expected $Status." }
    $response.Content | ConvertFrom-Json -AsHashtable -NoEnumerate
}

try {
    $env:MEMORY_MODEL_URL = ''
    $env:MEMORY_KEY_FILE = Join-Path $Fixture 'memory.key'
    $env:MEMORY_JOURNAL = Join-Path $Fixture 'memory.journal'
    $env:MEMORY_MYSQL_DSN = $env:MEMORY_TEST_DSN
    $previous = Invoke-Binary $old @('version')
    Invoke-Binary $old @('migrate')
    Invoke-Binary $old @('keygen')
    Invoke-Binary $old @('journal-init')
    $owner = Invoke-Binary $old @('provision','upgrade','taylor','owner','1')
    $first = Invoke-Binary $old @('provision','upgrade','assistant-a','app','1')
    $second = Invoke-Binary $old @('provision','upgrade','assistant-b','app','1')
    $process = Start-TestServer $old
    foreach ($app in @('assistant-a','assistant-b')) {
        $null = Invoke-API Post consent $owner @{ app=$app; purpose='support'; read=$true; write=$true; auto_accept=$true;
            expires=[datetime]::UtcNow.AddMinutes(30).ToString('o') }
    }
    $source = @{subject='taylor'; purpose='support'; session='legacy'; external_id='notice-01';
        kind='user'; occurred=[datetime]::UtcNow.AddMinutes(-3).ToString('o'); statement='Remember my notifications as email.'}
    $written = Invoke-API Post source $first $source
    $baseline = Invoke-API Get export $owner
    if ($written.status -ne 'applied' -or $baseline.facts.Count -ne 1 -or $baseline.facts[0].value -ne 'email') {
        throw 'The previous version did not persist the expected fact.'
    }
    Stop-Process -InputObject $process
    $process.WaitForExit()

    & $dumpTool @connection --single-transaction --hex-blob --set-gtid-purged=OFF --no-tablespaces "--result-file=$dump" memory_test
    if ($LASTEXITCODE) { throw 'Could not export the previous release database.' }
    $importPath = $dump.Replace('\','/')
    & $mysql @connection --database=memory_before "--execute=source $importPath"
    if ($LASTEXITCODE) { throw 'Could not restore the previous release database.' }
    $env:MEMORY_MYSQL_DSN = $env:MEMORY_TEST_RESTORE_DSN
    $before = & $current recover 2>&1
    if ($LASTEXITCODE -eq 0 -or "$before" -notmatch 'run agent-memory migrate') { throw 'The unupgraded database was not rejected.' }
    Invoke-Binary $current @('migrate')
    Invoke-Binary $current @('migrate')
    Invoke-Binary $current @('recover')
    $process = Start-TestServer $current
    $restored = Invoke-API Get export $owner
    if (($restored | ConvertTo-Json -Depth 12 -Compress) -cne ($baseline | ConvertTo-Json -Depth 12 -Compress)) {
        throw 'The export from the previous release changed after upgrading.'
    }
    $replay = Invoke-API Post source $first $source
    if (!$written.source -or !$replay.replayed -or $replay.source -cne $written.source) {
        throw 'The historical source did not deduplicate.'
    }
    $source.statement = 'Remember my notifications as paper mail.'
    $conflict = Invoke-API Post source $first $source 409
    $retry = Invoke-API Post source $first $source 409
    $queue = Invoke-API Get source-conflicts $owner
    if (!$conflict.conflict_id -or !$retry.replayed -or $conflict.conflict_id -ne $retry.conflict_id -or
        $queue.items.Count -ne 1 -or $queue.items[0].source -cne $written.source) { throw 'Stable conflict receipt missing after upgrade.' }
    foreach ($token in @($first,$second)) {
        $matches = Invoke-API Post recall $token @{subject='taylor'; purpose='support'; slot='notifications'}
        if ($matches.Count -ne 1 -or $matches[0].source.id -ne $written.source -or $matches[0].value -ne 'email') {
            throw 'An assistant saw an incorrect fact after upgrade.'
        }
    }
    $receipt = Invoke-API Post forget $owner @{slot='notifications'; key='upgrade-forget'}
    $null = Invoke-API Post source $first $source 410
    $queue = Invoke-API Get source-conflicts $owner
    if ($queue.items.Count -ne 0) { throw 'Forgetting left a queued conflict after upgrade.' }
    [ordered]@{previous=$previous; exact_export=$true; historical_source_replayed=$true; conflict_replay=$true;
        assistants_recalling_original=2; post_forget_queue_size=0; watermark=$receipt.watermark;
        backup_sha256=(Get-FileHash -LiteralPath $dump -Algorithm SHA256).Hash.ToLowerInvariant()} | ConvertTo-Json -Compress
} finally {
    if ($process -and !$process.HasExited) { Stop-Process -InputObject $process; $process.WaitForExit() }
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
}
