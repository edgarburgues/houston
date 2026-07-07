#!/bin/sh
# handler.sh — POSIX variant of handler.ps1, same contract: envelope JSON on
# stdin, HOUSTON_EVENT names the surface, one JSON object on stdout, nonzero
# exit on failure. The JSON handling below is deliberately crude sed/awk —
# good enough for these payloads; a real module should use a proper JSON
# tool (python, jq, ...).

body=$(cat)

case "$HOUSTON_EVENT" in
action.invoke)
  # The only "title" in an action envelope is the selected mission's.
  title=$(printf '%s' "$body" | sed -n 's/.*"title":"\([^"]*\)".*/\1/p')
  printf '{"status":"hello from \\"%s\\""}' "${title:-somewhere}"
  ;;
missions.transform)
  # Badge every mission tagged `wip`. Mission objects contain no nested
  # braces, so splitting records on "{" isolates one mission per record.
  patches=$(printf '%s' "$body" | awk '
    BEGIN { RS = "{"; sep = "" }
    /"tags":\[[^]]*"wip"/ && match($0, /"key":"[^"]*"/) {
      printf "%s{\"key\":\"%s\",\"badge\":\"WIP\"}", sep, substr($0, RSTART + 7, RLENGTH - 8)
      sep = ","
    }')
  printf '{"patches":[%s]}' "$patches"
  ;;
preview.append)
  project=$(printf '%s' "$body" | sed -n 's/.*"project":"\([^"]*\)".*/\1/p')
  printf '{"sections":[{"title":"Hello","body":"project: %s\\nsaid hello from handler.sh"}]}' "${project:-unknown}"
  ;;
statusline.segment)
  # Machine-global by contract: nothing session-specific may go here.
  printf '{"text":"hello %s"}' "$(date +%H:%M)"
  ;;
*)
  echo "unknown event: $HOUSTON_EVENT" >&2
  exit 3 # contract mismatch, per the documented exit-code convention
  ;;
esac
