# 🚀 Houston

Mission-control para **Claude Code**: equilibra varias cuentas detrás de un único
comando **y** te deja navegar, organizar y retomar tus conversaciones — todo en
una sola herramienta, una sola instalación.

## La idea

Claude Code (2.1.x) liga el login y el onboarding a la **cuenta autenticada**, no
solo al directorio de config. Para que varias cuentas convivan en modo interactivo
sin re-onboarding ni "Claude API" sin identidad, Houston da a cada cuenta su propio
`CLAUDE_CONFIG_DIR` y comparte los datos con enlaces nativos del SO:

- **Un config dir por cuenta** (`~/.claude-accounts/account-<id>`): login propio
  (`/login` una sola vez) y onboarding aislados. Así Claude muestra el email real
  de la cuenta.
- **Datos compartidos** (`projects`, `sessions`, `plugins`, `plans`, `todos`) y
  **personalizaciones de usuario** (`skills`, `commands`, `agents`, `workflows`,
  `rules`, `output-styles`, `themes`) enlazados a un store común `~/.claude-shared`
  mediante **junctions en Windows** y **symlinks en macOS/Linux** → cualquier
  cuenta ve y retoma **todas** las conversaciones, plugins, skills, subagentes y
  reglas, sin divergencia.
- **Lanzamiento equilibrado**: `houston run` sondea la cuota de cada cuenta (con su
  token de login) y elige la **menos cargada**, mostrando antes una tabla con el
  email y el uso 5h/7d. Prioriza las cuentas que aún no tienen login para que las
  configures, y permite forzar una con `-a <id>`.
- **Concurrencia segura**: cada terminal lleva su cuenta en *su* `CLAUDE_CONFIG_DIR`.
  Distintas terminales = distintas cuentas a la vez, sin pisarse.

## Instalación

```powershell
# desde el repo (descarga el binario de Releases y verifica su SHA-256;
# si no, compila con Go):
git clone https://github.com/edgarburgues/houston
cd houston/packaging && pwsh ./Install.ps1

# o zip: descarga houston-<ver>.zip de Releases (binario incluido)
#        descomprime  →  cd houston && pwsh ./Install.ps1
```

Multiplataforma (Windows / macOS / Linux), idempotente, sin privilegios. Instala
el binario y deja `houston` en el PATH.

Los binarios se publican en [Releases](https://github.com/edgarburgues/houston/releases)
junto a `checksums.txt`. El instalador **verifica el SHA-256** antes de instalar;
para comprobarlo a mano: `sha256sum -c checksums.txt`. Opciones útiles:
`./Install.ps1 -Version v0.3.0` (fija una versión), `-NoProfileEdit` (no toca el
perfil ni el PATH).

## Uso

```powershell
# 1) registrar cada cuenta (solo una etiqueta; el login es luego, por cuenta):
houston account add work
houston account add personal

# 2) preparar los config dirs por cuenta + enlaces compartidos (idempotente):
pwsh ./houston-setup-accounts.ps1

# 3) lanzar: la 1ª vez por cuenta se abrirá /login en el navegador (una sola vez)
houston run                     # cuenta menos cargada (o la que falte por login)
houston run -a work2            # fuerza una cuenta concreta
houston run --resume <id>       # cualquier otro arg se pasa a claude

# 4) navegar y retomar conversaciones (TUI):
houston
```

El instalador deja además un alias en tu perfil para que `claude` se sienta
normal pero lo orqueste Houston: `claude ...` ≡ `houston run ...` (elige la
cuenta menos cargada, fija su `CLAUDE_CONFIG_DIR` y lanza el `claude` real).
Es una función de shell, así que no choca con el binario `claude` del PATH.
Para buscar/retomar conversaciones, usa `houston`.

### Comandos de cuentas
| Comando | Acción |
|---|---|
| `houston account add <etiqueta>` | registra una cuenta (solo la etiqueta; el login se hace en el primer `houston run`) |
| `houston account ls` | lista cuentas con su email y presión de cuota (5h / 7d) |
| `houston account rm <id>` | elimina una cuenta |
| `houston run -a <id>` | lanza forzando una cuenta concreta |
| `houston doctor` | audita el layout (enlaces, logins, dirs sin compartir) |
| `houston doctor --fix` | repara el layout de forma idempotente (no pisa datos) |

## La TUI

| Término | Qué es |
|---|---|
| **Misión** | Una conversación (`.jsonl`). |
| **Programa** | Agrupación lógica de misiones (manifiesto `.prog`). |

Atajos: `↑↓`/`jk` mover · `tab`/`←→` panel · `/` buscar · `enter` **resume** ·
`*` fijar · `a` archivar · `t` tag · `n` nota · `p`→programa · `P` nuevo · `x`
quitar · `e` export · **`A` cuentas** · `r` reindex · `?` ayuda · `q` salir.

El **resume** hace `cd` al directorio correcto (resuelve incluso nombres con
guiones ambiguos) y lanza `claude --resume` con la cuenta elegida — adiós al
"No conversation found".

## Auto-gestión: `houston doctor`

El layout multi-cuenta (store compartido + un dir por cuenta + enlaces) puede
derivar con el tiempo: enlaces que faltan, una carpeta real donde debería ir un
enlace, una cuenta sin login. `houston doctor` lo **audita** y `houston doctor
--fix` lo **repara**, de forma idempotente y multiplataforma (junctions en
Windows sin admin, symlinks en macOS/Linux). Es la versión en Go —y portable— de
lo que hacía `houston-setup-accounts.ps1`.

Nunca pisa datos: si encuentra una carpeta real **con contenido** donde debería ir
un enlace, la deja intacta y te dice que la fusiones a mano. Crea los dirs que
falten en el store compartido y enlaza en cada cuenta los que falten.

## Statusline (cuenta + cuota dentro de Claude)

Houston puede pintar la **cuenta activa y su cuota 5h/7d** en la barra de estado
de Claude Code, leyendo los `rate_limits` que Claude pasa por stdin (sin `jq` ni
dependencias: lo parsea el propio binario). En el `settings.json` compartido:

```json
{ "statusLine": { "type": "command", "command": "houston statusline" } }
```

Muestra algo como `🚀 work2 · work2@example.com · 5h 41% · 7d 7% · Opus 4.8 · ctx 12%`.

## Estructura

```
houston/
├── .github/workflows/
│   └── release.yml        tag v* → cross-compila + Release con checksums
├── packaging/
│   ├── Install.ps1        instalador idempotente, multiplataforma
│   ├── Uninstall.ps1      revierte la instalación (sin tocar conversaciones)
│   └── Build.ps1          (mantenimiento) cross-compila + genera el zip
├── LICENSE                MIT
├── internal/
│   ├── accounts/  usage/  launch/     cuentas, sondeo de cuota, lanzamiento
│   ├── provision/                     layout multi-cuenta (doctor: audita/repara)
│   ├── statusline/                    barra de estado: cuenta activa + cuota
│   ├── scan/  model/  pathenc/        descubrimiento e indexado de misiones
│   ├── resume/  export/               retomar y exportar (balanceado)
│   └── tui/                           interfaz (misiones + cuentas)
└── main.go · go.mod
```

## Desinstalación

```powershell
pwsh packaging/Uninstall.ps1            # quita binario, bloques del perfil y PATH
pwsh packaging/Uninstall.ps1 -PurgeData # además borra el store de Houston
```

**No toca tus conversaciones**: los datos compartidos (`~/.claude-shared`), los
logins por cuenta (`~/.claude-accounts`) y el store (`~/.claude/houston`) se
conservan. El propio script imprime cómo borrarlos a mano si lo deseas.

## Build (mantenimiento)

```powershell
pwsh packaging/Build.ps1     # cross-compila 6 plataformas + zip local
go test ./...                # tests
```
Go ≥ 1.26, sin cgo. Dependencias: Bubble Tea / Bubbles / Lip Gloss.

Publicar una versión: empuja un tag `v*` (p.ej. `git tag v0.3.0 && git push origin
v0.3.0`). El workflow `release.yml` cross-compila las 6 plataformas, genera
`checksums.txt` y crea la GitHub Release con todo adjunto.

## Licencia

[MIT](LICENSE) © 2026 Edgar Fernández Diéguez.
