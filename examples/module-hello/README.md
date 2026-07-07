# module-hello

The smallest Houston module that touches every surface — install it, poke
it, copy it. Full reference: [docs/modules.md](../../docs/modules.md).

| Surface | What it does |
|---|---|
| action (`H`, missions screen) | echoes the selected mission's title into the status footer |
| missions transform | badges every mission tagged `wip` with ` [WIP]` |
| preview | appends a "Hello" section for the selected mission |
| statusline segment | a `hello HH:mm` clock, cached 60 s machine-wide |
| theme | shifts the TUI accent color to ANSI-256 `75` |

```
houston module add ./examples/module-hello   # snapshot install — lands DISABLED
houston module test module-hello             # run every handler against fixtures
houston module enable module-hello           # consent point: code runs from here on
```

One script, `handler.ps1`, serves all four events by dispatching on
`$env:HOUSTON_EVENT`. Commands are explicit argv arrays — nothing is
inferred from file extensions — so the shipped POSIX variant `handler.sh`
runs only if you edit `module.json` and swap every command for
`["sh", "handler.sh"]` (the `"//"` key in the manifest is a plain unknown
field, ignored by Houston, standing in for the comment JSON cannot carry).

The handlers are deliberately tiny: read the envelope from stdin, print one
JSON object on stdout, exit 0. Edit them, re-run `houston module test
module-hello`, and watch the field-by-field verdict.
