<div align="center">

# vector

**El límite de alcance para agentes de código.** Una capa de control determinística que compila un alcance declarado en una denegación dura, audita lo que realmente cambió con una operación de conjuntos que no puede fallar, y se corre del camino.

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-100-green.svg)](#desarrollo)
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

`init` detecta el stack, escribe `.vector/policy.toml` y registra sus hooks en `.claude/settings.json` — mergeando con lo que ya haya, nunca reemplazándolo. `vector uninstall` saca exactamente esas entradas y deja el resto intacto.

**Eso es todo el setup.** Seguí corriendo `claude` como siempre. Nunca declarás un alcance, nunca pasás un id de tarea, nunca te tenés que acordar de auditar. Usá `--no-hooks` si preferís cablearlo a mano.

> La salida de la CLI está en inglés. Los ejemplos de consola de abajo la muestran tal cual es.

---

## Comandos

| comando | qué hace |
| --- | --- |
| `vector init` | detecta el stack, escribe la config y registra los hooks |
| `vector uninstall` | saca los hooks, dejando los demás intactos |
| `vector doctor` | verifica que vector esté haciendo algo |
| `vector audit` | compara el working tree contra el límite |
| `vector verify` | corre los checks del proyecto y da un veredicto completo |

Estos los corre el agente; vos casi nunca:

| comando | qué hace |
| --- | --- |
| `vector scope new <id> -w <pat>` | declara el alcance de una tarea y lo activa |
| `vector scope expand <id> -w <pat>` | lo ensancha, con motivo y evidencia |
| `vector scope list` | lista los alcances declarados |
| `vector observe "<nota>"` | registra algo notado, sin actuar sobre ello |
| `vector observe list` | lista las observaciones registradas |

### Una sesión

Vos escribís dos palabras. Todo lo demás son los hooks.

```console
$ claude
> agregá un filtro por fecha al dashboard
```

`SessionStart` le pasa al agente los comandos reales del proyecto y una sola instrucción: declará tu límite cuando lo sepas.

```
vector is active in this repository.
Package manager: pnpm. Verification — test: pnpm run test; build: pnpm run build.
No scope is declared. Once you know which files this task needs — after
exploring, before your first edit — declare it:
  vector scope new <short-id> -o "<objective>" -w "<path pattern>"
```

**El alcance lo declara el agente, no vos.** Ese orden es todo el punto: después de explorar sabe qué archivos necesita la tarea, y antes de explorar no lo sabe nadie. Pedirle a una persona que prediga rutas de antemano es pedirle que haga el trabajo por el que abrió el agente.

Después `PreToolUse` decide cada escritura. Dentro del alcance, no dice nada:

```console
$ # Write src/dashboard/Filter.tsx  →  (silencio)
```

Fuera del alcance, reporta y ofrece las dos salidas honestas:

```
vector: src/middleware/auth.ts is outside the declared scope. Continuing
(advisory mode). If this belongs to the task, run `vector scope expand
date-filter -w "<pattern>" -reason blocking -evidence "<what proves it>"`;
if it does not, leave it and record it with `vector observe "<note>"`.
```

Y una ruta prohibida se deniega de una, incluso a través de la shell — la forma que se le escapa a un hook que solo mira `Edit` y `Write`:

```console
$ # Bash: cat > .env << EOF
vector: .env is forbidden by .env
```

`Stop` corre el audit al cerrar el turno, así ves el resultado sin pedirlo.

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
| **T3** confinamiento | sandbox del SO — **lo que configura `vector init -sandbox`** | absoluta — sobrevive a un hook evadido, y cubre escrituras que vector no ve |
| **T5** intercepción | hook nativo `PreToolUse` — **lo que registra `vector init`** | alta, con [~5 % de fugas documentadas](https://github.com/anthropics/claude-code/issues/45427) |
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

## El veredicto

`audit` responde a dónde fue el cambio. `verify` agrega si funciona, corriendo los comandos que el proyecto ya declara — y combina los dos:

```console
$ vector verify
PARTIALLY_VERIFIED — passed: lint, test, build — but no scope was declared,
                     so conformance was not checked

  ---- typecheck  not declared by the project
  ok   lint       go vet ./...     (101ms)
  ok   test       go test ./...    (2.307s)
  ok   build      go build ./...   (254ms)
```

Dos reglas deciden el veredicto:

**Nada es `VERIFIED` si no corrió algo.** Un repositorio que no declara comando de tests no demostró que funciona, por más verde que esté el resto. "No chequeado" nunca se vuelve "está bien".

**El alcance le gana a la evidencia.** Un cambio que pasa todos los tests pero tocó archivos que nadie declaró sigue siendo `OUT_OF_SCOPE`. Los tests que pasan no autorizan el trabajo retroactivamente.

| veredicto | significado | exit |
| --- | --- | --- |
| `VERIFIED` | en alcance, y pasó cada check declarado | 0 |
| `PARTIALLY_VERIFIED` | lo que corrió pasó, pero falta algo chequeable | 0 |
| `FAILED` | un check devolvió distinto de cero | 1 |
| `OUT_OF_SCOPE` | el cambio se fue del límite — se reporta aunque los checks pasen | 1 |
| `UNVERIFIED` | no corrió nada | 1 |

Los checks corren de más barato a más caro —typecheck, lint, test, build— porque un error de tipos explica los fallos de test que vendrían después. Cada uno tiene timeout, así una suite colgada falla ruidosamente en vez de colgarse.

`verify` no lo corre ningún hook. Correr la suite de tests al final de cada turno costaría más que el desperdicio que evita; `Stop` corre el audit barato y te deja a vos o a CI la pregunta cara.

## Códigos de salida

| comando | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | en alcance, sin alcance declarado, o sin cambios | fuera de alcance, o ruta prohibida tocada | error de uso o configuración |
| `doctor` | ningún check falló | un check falló | no es un repositorio |

Ambos aceptan `-json` y emiten un schema versionado (`vector.audit/v1`, `vector.doctor/v1`). **Gateá por el campo JSON, nunca por la presencia de salida.**

---

## Estado

Funcionando y usado sobre sí mismo: 11 comandos, 100 tests, cero llamadas a modelos, dos dependencias.

Claude Code es el único agente cuyos hooks escribe `init` hoy. Codex, Cursor y Gemini exponen el mismo primitivo con otros nombres de evento, así que los adapters son traducción y no arquitectura nueva — pero no están escritos, y `doctor` va a reportar T2 honestamente en esos.

Sigue abierto: los adapters de hooks de Codex, Cursor y Gemini; la caducidad del veredicto, para que un verify que pasó expire cuando el código sobre el que afirmaba se mueve; y un presupuesto de intentos, para que una tarea que gira contra una pared lo diga.

---

## Referencia

[`docs/reference.md`](docs/reference.md) es la referencia completa: qué es requerido
y qué apenas recomendado, cada comando con sus flags y códigos de salida, cada
archivo que vector escribe y si va o no a git, y qué nivel de enforcement se alcanza
realmente en cada agente. Está en inglés, como el resto de los artefactos técnicos.

## Desarrollo

```bash
go test ./...        # 100 tests
go vet ./...
gofmt -l .
```

Dos dependencias: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) y [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — esta última porque el `filepath.Match` de la biblioteca estándar no soporta `**`.

---

<div align="center">

[English](README.md) · **Español**

</div>
