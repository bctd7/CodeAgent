# Claude-compatible PreToolUse example. The host invokes this script with JSON
# on stdin. It accepts both our path field and Claude's file_path field.
$ErrorActionPreference = 'Stop'
$utf8 = [System.Text.UTF8Encoding]::new($false)
[Console]::InputEncoding = $utf8
[Console]::OutputEncoding = $utf8
$inputEvent = [Console]::In.ReadToEnd() | ConvertFrom-Json

if ($inputEvent.hook_event_name -ne 'PreToolUse') {
    throw 'This script handles PreToolUse only'
}

if ($inputEvent.tool_name -in @('write', 'edit')) {
    $target = $inputEvent.tool_input.path
    if (-not $target) { $target = $inputEvent.tool_input.file_path }
    if ($target -and [System.IO.Path]::GetFileName($target) -eq '.env') {
        $outputEvent = @{
            hookSpecificOutput = @{
                hookEventName = 'PreToolUse'
                permissionDecision = 'deny'
                permissionDecisionReason = 'Project policy prevents editing .env through this tool'
            }
        }
        [Console]::Out.Write(($outputEvent | ConvertTo-Json -Depth 5 -Compress))
    }
}

# No decision means the host's normal permission flow, not automatic approval.
exit 0
