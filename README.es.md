<div align="center">

# vector

**El límite de alcance para agentes de código.** Una capa de control determinística que compila un alcance declarado en una denegación dura, audita lo que realmente cambió con una operación de conjuntos que no puede fallar, y se corre del camino.

[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-42-green.svg)](#desarrollo)
[![Estado](https://img.shields.io/badge/estado-MVP-orange.svg)](#estado)
[![Determinístico](https://img.shields.io/badge/llamadas%20a%20modelo-cero-black.svg)](#qué-no-es-vector)

[English](README.md) · **Español**

</div>

---

## Qué es

A un agente al que le pedís un filtro por fecha en el dashboard, a veces también te refactoriza el middleware de auth. vector lo hace visible y —cuando llegue el hook— lo frena antes de que toque el disco.

```
vos                          tu agente                       vector
├── "agregá un filtro"  ──►  Claude Code       ──────────►  ┌──────────────┐
│                            Codex · Cursor                 │   ALCANCE    │  rutas declaradas
│                            Gemini · OpenCode              │   EVIDENCIA  │  git diff
└── vector audit        ◄──  (sin modificar)   ◄──────────  │  VEREDICTO   │  exit 0 / 1
                                                            └──────────────┘
```

Tres primitivas, ninguna más. **Alcance** es un conjunto de rutas escribibles. **Evidencia** es lo que git dice que pasó. **Veredicto** es la diferencia entre ambos.

vector nunca ejecuta tu agente, nunca sostiene contexto, nunca enruta skills y nunca recuerda nada. Responde una sola pregunta: *¿el cambio se quedó dentro del límite declarado para él?*

---

## El problema

Está medido, no es anécdota. [OverEager-Bench](https://arxiv.org/abs/2605.18583) corrió 500 escenarios sobre ~7.500 ejecuciones:

| Setup | Tasa de acciones fuera de alcance |
| --- | --- |
| Claude Code · Codex CLI · Gemini CLI | **5,4 % – 27,7 %** |
| OpenHands con confirmación explícita | 0,2 % – 4,5 % |

Un orden de magnitud, decidido por si algo pregunta o no. Y el prompting solo no lo cierra: con instrucciones explícitas de no hacerlo, los modelos igual seleccionaron herramientas no autorizadas en [48–68 % de escenarios adversariales](https://arxiv.org/pdf/2605.18414), 96 % bajo escalación de rol. Las allowlists escritas dentro del prompt bajan las violaciones al 4 % — nunca a cero. Una compuerta fuera del prompt llega a 0 % por construcción.

Esa brecha es toda la razón por la que vector existe.

---

## Qué **no** es vector

Agresivamente, y a propósito:

- **No es un runtime de agente.** Sin loop de LLM, sin loop de tool-use, sin ventana de contexto.
- **No es un sistema de memoria.** Tu repositorio es la fuente de verdad.
- **No es una base de conocimiento, un índice RAG ni un store de embeddings.**
- **No es un framework de specs.** OpenSpec y Spec Kit ya existen.
- **No es un orquestador multi-agente.**
- **No es un revisor con IA.** Auditar código generado con más generación es el fallo, no la solución.
- **No es un daemon.** No corre nada en segundo plano.

Hay **cero llamadas a modelos** en todo vector. Cada decisión es una operación de conjuntos sobre rutas normalizadas, y cada respuesta nombra la regla que la produjo.

---

## Instalación

```bash
go install github.com/3zequiel3/vector/cmd/vector@latest
```

Después, una vez por repositorio:

```bash
cd tu-proyecto
vector init
```

Eso es todo el setup. Seguí usando `claude`, `codex`, `cursor` o lo que ya uses — vector no se mete en tu flujo.

> La salida de la CLI está en inglés. Los ejemplos de consola de abajo la muestran tal cual es.

---

## Comandos

| comando | qué hace |
| --- | --- |
| `vector init` | detecta el stack y escribe `.vector/policy.toml` |
| `vector scope new <id> -w <pat>` | declara el alcance de una tarea |
| `vector scope expand <id> -w <pat>` | lo ensancha, con motivo y evidencia |
| `vector scope list` | lista los alcances declarados |
| `vector observe "<nota>"` | registra algo notado, sin actuar sobre ello |
| `vector observe list` | lista las observaciones registradas |
| `vector audit [-task <id>]` | compara el working tree contra el límite |
| `vector doctor` | verifica que vector esté haciendo algo |

### Una sesión

```console
$ vector scope new filtro-fecha -o "agregar filtro por fecha al dashboard" \
    -w "src/dashboard/**" -w "src/dashboard/__tests__/**"
.vector/scope/filtro-fecha.toml written

$ claude                                    # tu flujo de siempre, intacto
> agregá un filtro por fecha al dashboard

$ vector audit -task filtro-fecha
OUT OF SCOPE — 2 of 9 file(s) were not declared
  objective: agregar filtro por fecha al dashboard
  out_of_scope   src/middleware/auth.ts
  out_of_scope   src/lib/session.ts
```

Dos archivos que nadie pidió. Ese número es lo que esta herramienta existe para producir.

---

## Inteligencia de versiones

`vector init` nunca asume un stack, y mantiene cuatro verdades separadas, porque colapsarlas en una sola "versión" es cómo una herramienta termina segura y equivocada:

| | fuente | autoridad |
| --- | --- | --- |
| **declarado** | manifiesto, `engines`, `packageManager`, `.nvmrc` | intención — no es verdad |
| **resuelto** | la identidad del lockfile elige el package manager | qué se instalaría |
| **instalado** | `node_modules/`, `.venv`, `--version` | **qué corre de verdad** |
| **upstream** | el registry | deliberadamente no se consulta |

```console
$ vector init
package manager        pnpm  (lockfile: pnpm-lock.yaml)
  installed            11.8.0
runtime declared       24.x
runtime installed      24.17.0

frameworks            declared         installed
  next                 16.2.12          16.2.12
  react                19.2.4           19.2.4
  tailwindcss          ^4               4.3.3

test                   pnpm run test
typecheck              pnpm run typecheck
```

La detección del package manager sube desde el directorio de trabajo hasta la raíz del repositorio —nunca más allá— aplicando las mismas estrategias ordenadas en cada nivel: lockfile, después `packageManager`, después `devEngines`, después metadata de instalación. Los comandos de verificación se leen de los manifiestos del propio proyecto. vector los invoca; nunca los inventa.

Un rango como `^4` contra un `4.3.3` instalado **no** se reporta como desacuerdo. vector solo afirma un mismatch que puede probar sin un solver de semver: un pin exacto que difiere.

---

## El alcance, y cómo se le permite crecer

Encontrar un problema no autoriza a arreglarlo. Un límite se ensancha por uno de cinco motivos, y solo con evidencia:

`blocking` · `security` · `invariant` · `verification` · `authorized`

```console
$ vector scope expand filtro-fecha -w "src/api/types.ts" -reason blocking
vector: policy requires evidence (expansion_requires_evidence = true);
        pass -evidence with what proves "blocking" applies
```

Con evidencia, la expansión se **anexa, nunca se mergea** a la declaración original:

```toml
objective = "agregar filtro por fecha al dashboard"

write = ["src/dashboard/**"]              # the original declaration, intact

[[expansion]]
at = "2026-09-07T08:17:09Z"
reason = "blocking"  # prevents completing the requested task
evidence = "pnpm run typecheck fails: src/api/types.ts does not export DateFilter"
write = ["src/api/types.ts"]
```

El archivo se vuelve el registro de cómo creció el límite y sobre qué base — revisable, en git. Editar `write` in-place borraría justamente esa historia.

### Observaciones

Todo lo demás que el agente notó va acá, y se queda acá:

```console
$ vector observe "el middleware de auth mezcla sesión y token en la misma capa" \
    -category architecture -severity medium
OBS-001 recorded (architecture/medium) — action: defer
```

`acción: defer` no es un parámetro. Registrar es la totalidad de la respuesta permitida.

---

## Niveles de enforcement

vector es honesto sobre qué nivel alcanza realmente, y `vector doctor` lo reporta:

| nivel | mecanismo | garantía |
| --- | --- | --- |
| **T3** confinamiento | sandbox del SO (Seatbelt, bubblewrap) | absoluta — sobrevive a un hook evadido |
| **T5** intercepción | hook nativo `PreToolUse` | alta, con [~5 % de fugas documentadas](https://github.com/anthropics/claude-code/issues/45427) |
| **T2** observación | `git diff` contra el límite | **la detección es total, la prevención es nula** |
| **T1** consejo | `AGENTS.md`, `CLAUDE.md` | ninguna |

Contra la intuición, **T2 es más confiable que T5**: un hook tiene fugas medidas —subagentes, heredocs en Bash, fallos silenciosos— mientras que una operación de conjuntos sobre git no puede fallar. No previene, pero nunca miente.

```console
$ vector doctor
REPOSITORY
  ok   self-protection    vector's own config is outside the writable scope
SCOPES
  warn filtro-fecha       patterns matching no file in the repo: src/dashbaord/**
                          → usually a typo; check the path
ENFORCEMENT
  warn Claude Code        installed, no vector hook registered
       tier               T2 observation — detects 100% after the fact,
                          but prevents nothing

0 failure(s), 2 warning(s) — enforcement at T2
```

Nada en vector reporta jamás "seguro". Lo más fuerte que afirma es qué nivel se alcanzó de verdad.

---

## Códigos de salida

| comando | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | en alcance, sin alcance declarado, o sin cambios | fuera de alcance, o ruta prohibida tocada | error de uso o configuración |
| `doctor` | ningún check falló | un check falló | no es un repositorio |

Ambos aceptan `-json` y emiten un schema versionado (`vector.audit/v1`, `vector.doctor/v1`). **Gateá por el campo JSON, nunca por la presencia de salida.**

---

## Estado

El núcleo determinístico está completo y se usa sobre sí mismo: 8 comandos, 42 tests, cero llamadas a modelos.

El hook `PreToolUse` —la pieza que mueve el enforcement de T2 a T5— **todavía no está construido**, deliberadamente. Son tres semanas de trabajo justificadas por un número que nadie midió: la línea base de acciones fuera de alcance en repositorios reales. `vector audit` es el instrumento de esa medición, y por eso vino primero.

Hasta entonces, vector detecta. No previene, y lo dice.

---

## Desarrollo

```bash
go test ./...        # 42 tests
go vet ./...
gofmt -l .
```

Dos dependencias: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) y [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — esta última porque el `filepath.Match` de la biblioteca estándar no soporta `**`.

---

<div align="center">

[English](README.md) · **Español**

</div>
