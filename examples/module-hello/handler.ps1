# handler.ps1 — module-hello's single handler for every surface. Houston
# names the surface in HOUSTON_EVENT and writes the envelope (one JSON
# object, then EOF) to stdin; the reply is ONE JSON object on stdout.
# Rules of thumb this follows (docs/modules.md): the manifest declares
# -NoProfile, the reply is ConvertTo-Json -Compress and nothing else on
# stdout, diagnostics go to stderr, and it targets pwsh 7+ — on Windows
# PowerShell 5, Write-Host and `>` redirection corrupt the reply stream.

$envelope = [Console]::In.ReadToEnd() | ConvertFrom-Json

switch ($env:HOUSTON_EVENT) {
  'action.invoke' {
    # Echo the selected mission's title into the TUI status footer.
    $title = $envelope.payload.mission.title
    @{ status = "hello from `"$title`"" } | ConvertTo-Json -Compress
  }
  'missions.transform' {
    # Badge every mission tagged `wip`. Patches are sparse: rows without a
    # patch are left alone, and an empty patches array is a valid no-op.
    $patches = @(
      foreach ($m in $envelope.payload.missions) {
        if ($m.tags -contains 'wip') { @{ key = $m.key; badge = 'WIP' } }
      }
    )
    @{ patches = $patches } | ConvertTo-Json -Compress -Depth 5
  }
  'preview.append' {
    $m = $envelope.payload.mission
    $body = "project: $($m.project)`nmessages: $($m.userMsgs) user / $($m.assistantMsgs) assistant"
    @{ sections = @(@{ title = 'Hello'; body = $body }) } | ConvertTo-Json -Compress -Depth 5
  }
  'statusline.segment' {
    # Machine-global by contract: nothing session-specific may go here.
    @{ text = 'hello ' + (Get-Date -Format 'HH:mm') } | ConvertTo-Json -Compress
  }
  default {
    [Console]::Error.WriteLine("unknown event: $env:HOUSTON_EVENT")
    exit 3 # contract mismatch, per the documented exit-code convention
  }
}
