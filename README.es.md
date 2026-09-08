<div align="center">

# vector

**Mantiene a un agente de código dentro de la tarea que pediste.**

Pedís un filtro por fecha. El agente además te refactoriza el middleware de auth.
vector lo nota, te lo dice y —donde puede— lo frena antes.

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-247-green.svg)](#desarrollo)
[![Estado](https://img.shields.io/badge/estado-MVP-orange.svg)](#estado)
[![Determinístico](https://img.shields.io/badge/llamadas%20a%20modelo-cero-black.svg)](#qué-no-es-vector)

[English](README.md) · **Español**

</div>

---

## Qué hace

Le pedís una cosa a tu agente. En el camino toca además tres archivos que nadie
mencionó. A veces es necesario y a veces es deriva, y hoy nada te dice cuál de
las dos fue hasta que leés el diff vos mismo.

vector vigila el límite de una tarea y responde dos preguntas:

- **¿El cambio se quedó donde tenía que quedarse?**
- **¿Realmente funciona?**

Nunca ejecuta tu agente, nunca sostiene contexto, nunca le habla a un modelo.
Cada respuesta es una comparación que te puede mostrar.

---

## Instalación

```bash
curl -fsSL https://raw.githubusercontent.com/3zequiel3/vector/main/install.sh | sh
```

No hace falta tener Go: baja un binario con checksum y se niega a instalar uno
que no pueda verificar. Si el directorio de instalación no está en tu `PATH`, el
script te imprime la línea exacta y en qué archivo ponerla.

> **Todavía no.** No hay ningún release taggeado, así que este comando no tiene
> nada que bajar. Hasta que salga el primer tag, usá `go install` de abajo.

<details>
<summary>Otras formas de instalarlo</summary>

```bash
go install github.com/3zequiel3/vector/cmd/vector@latest   # necesita Go 1.27+
```

Desde el fuente: `git clone` y después `go build ./cmd/vector`.

`go install` deja el binario en `~/go/bin`, que muchas veces no está en el
`PATH`. El instalador por curl existe en parte para evitar eso.

</details>

Después, una vez por repositorio:

```bash
cd tu-proyecto
vector init
```

Eso es todo el setup. **Seguí corriendo `claude` exactamente como siempre.**

<details>
<summary>Qué le hace <code>vector init</code> a tu repo</summary>

- Detecta tu stack —package manager, runtime, frameworks y los comandos de
  test/build/lint del propio proyecto— y los escribe en `.vector/policy.toml`
- Registra tres hooks en `.claude/settings.json`, **mergeando** con lo que ya
  haya en vez de reemplazarlo
- Nada más. `vector uninstall` saca exactamente esas entradas.

Agregá `-sandbox` para que además el SO deniegue escrituras a rutas prohibidas,
y `-no-hooks` para saltear el registro de hooks por completo.

</details>

---

## Qué pasa después

Nada que tengas que hacer. Este es el ciclo completo.

**1. Trabajás normal.**

```console
$ claude
> agregá un filtro por fecha al dashboard
```

**2. El agente declara qué va a tocar** — no vos. Lo hace después de explorar y
antes de su primera edición, que es el único momento en que alguien lo sabe.
vector frena esa primera escritura y se lo pide:

```
vector: no scope is declared, and this writes src/dashboard/Filter.tsx.
Declare the boundary before writing, covering every file this task needs:
  vector scope new <short-id> -o "<objective>" -w "<pattern>"
```

**3. Las escrituras dentro del límite son silenciosas.** Las de afuera se reportan:

```
vector: src/middleware/auth.ts is outside the declared scope. Continuing
(advisory mode). If this belongs to the task, run `vector scope expand ...`;
if it does not, leave it and record it with `vector observe "<note>"`.
```

**4. Al cerrar el turno te llega el resumen** sin pedirlo.

Listo. Escribiste `vector init` una vez y después una frase a tu agente.

> La salida de la CLI está en inglés. Los ejemplos de consola la muestran tal
> cual es.

<details>
<summary>Correrlo a mano</summary>

```bash
vector audit      # ¿el cambio se quedó en alcance?
vector verify     # ...¿y funciona? corre el test/build/lint del propio proyecto
vector doctor     # ¿vector está haciendo algo en esta máquina?
```

`vector doctor` es el que hay que correr cuando algo huele raro. Reporta qué
verificó, nunca que las cosas "están bien".

</details>

---

### Quién tipea qué

Vale la pena ser preciso acá, porque toda la premisa de vector es que **vos no
sos quien lo corre**.

| | quién | cuándo |
| --- | --- | --- |
| `vector init` | **vos** | una vez por repositorio |
| `vector scope new` | el agente | en su primera escritura, porque el hook lo frena y se lo pide |
| `vector scope expand` | el agente | cuando el hook reporta una escritura fuera del límite |
| `vector observe` | el agente | cuando el hook le dice que registre en vez de actuar |
| el audit de alcance | el hook `Stop` | cada turno, automático |
| señales de caducidad y reintentos | el hook `Stop` | cada turno, automático |
| **`vector verify`** | **vos** | **nunca automático — ver abajo** |

Todo lo que corre el agente lo dispara un hook denegando o reportando algo, así
que pasa esté el agente de humor o no.

`verify` es la única excepción, y es a propósito: correr la suite de tests al
final de cada turno costaría más que la deriva que evita. Por eso, cuando un
cambio está en alcance y nadie lo verificó, el hook `Stop` lo dice en vez de
dejar que el silencio se lea como una respuesta:

```
vector: date-filter is in scope, and nothing has checked whether it works.
Run `vector verify` for a verdict; until then the change is UNVERIFIED.
```

---

## Por qué existe

Esto no es una corazonada. Está medido. [OverEager-Bench](https://arxiv.org/abs/2605.18583) corrió 500 escenarios sobre ~7.500 ejecuciones:

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

Cuatro reglas deciden el veredicto, y cada una cierra una forma distinta de parecer terminado:

**Nada es `VERIFIED` si no corrió algo.** Un repositorio que no declara comando de tests no demostró que funciona, por más verde que esté el resto. "No chequeado" nunca se vuelve "está bien".

**El alcance le gana a la evidencia.** Un cambio que pasa todos los tests pero tocó archivos que nadie declaró sigue siendo `OUT_OF_SCOPE`. Los tests que pasan no autorizan el trabajo retroactivamente.

**Nada es `VERIFIED` si el cambio editó a su propio juez.** Una suite con las aserciones borradas sale con código cero. Una suite borrada entera sale cero, fuerte y en verde — y METR observó reward hacking explícito en 39 de 128 corridas de o3 sobre RE-Bench sin pedirlo, con exactamente esos mecanismos. Así que cuando un cambio es sustractivo neto sobre los archivos de test del proyecto, el veredicto tiene techo y lo dice:

```console
PARTIALLY_VERIFIED — passed: lint, test, build — but this change removed
6 line(s) from 1 test file and added 0 (total_test.go), so the suite that
passed is not the suite that was there
```

Acá no hay parser ni contador de aserciones — eso es por lenguaje y se pudre. Lee `git diff --numstat`, que git ya había calculado. Nunca hace fallar un build: sacar líneas de un test es algo normal, y vector no puede distinguir una consolidación de un vaciado.

**Un límite que no habla de migraciones no autoriza una.** Algunas rutas se escriben a propósito o no se escriben: migraciones, workflows de CI, terraform, material de claves. Cuando un cambio toca una y ningún patrón declarado era *sobre* ella, el veredicto tiene techo y la ruta se nombra. `migrations/**` declara una migración; `src/**` no, por más migraciones que vivan bajo `src`.

| veredicto | significado | exit |
| --- | --- | --- |
| `VERIFIED` | en alcance, y pasó cada check declarado | 0 |
| `PARTIALLY_VERIFIED` | lo que corrió pasó, pero falta algo chequeable | 0 |
| `FAILED` | un check devolvió distinto de cero | 1 |
| `OUT_OF_SCOPE` | el cambio se fue del límite — se reporta aunque los checks pasen | 1 |
| `UNVERIFIED` | no corrió nada | 1 |

Los checks corren de más barato a más caro —typecheck, lint, test, build— porque un error de tipos explica los fallos de test que vendrían después. Cada uno tiene timeout, así una suite colgada falla ruidosamente en vez de colgarse.

### Cuando un veredicto deja de ser cierto

`VERIFIED` es una afirmación sobre un árbol. Seguís editando y deja de serlo, así
que el hook `Stop` lo dice:

```
vector: the VERIFIED verdict for date-filter is stale — src/dashboard/Filter.tsx
changed since it was reached. What is on disk now is UNVERIFIED, not failed:
nothing is blocked and no exit code changed.
```

Stale no es failed. El vocabulario ya tenía una palabra para *desconocido* y es
esa. Solo se reportan stale los veredictos que pasaron — un `FAILED` viejo no
engaña a nadie, y la edición que lo volvió viejo **es** la respuesta que pedía.

El identificador es el blob hash del propio git, así que no se inventa nada
nuevo ni hace falta ninguna otra herramienta.

### Cuando una tarea falla una y otra vez

`verify` y el hook `Stop` lo dicen:

```
vector: 4 failed verifies in a row on date-filter, with no passing run in
between. The diff grew from 99 to 812 changed lines across them. Nothing is
blocked — vector cannot tell a productive attempt from an unproductive one.
Consider stopping and asking a human what to change.
```

Tres fallos consecutivos es el umbral: uno es trabajo, dos es la corrección
ordinaria en la que aterriza casi cualquier arreglo, y tres es el primero que el
patrón de dos intentos no explica. Una corrida que pasa corta la racha. Nunca se
bloquea nada — es una señal de que puede haber un loop, y una herramienta que
frena trabajo legítimo por una heurística se desinstala.

`verify` no lo corre ningún hook. Correr la suite de tests al final de cada turno costaría más que el desperdicio que evita; `Stop` corre el audit barato y te deja a vos o a CI la pregunta cara.

**En CI, pasá `-base`.** Un checkout limpio tiene el árbol de trabajo idéntico a `HEAD`, así que todo diff da vacío y una rama que vació sus tests hace tres commits parece no haber cambiado nada:

```bash
vector verify -base origin/main -json
```

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

## Códigos de salida

| comando | 0 | 1 | 2 |
| --- | --- | --- | --- |
| `audit` | en alcance, sin alcance declarado, o sin cambios | fuera de alcance, o ruta prohibida tocada | error de uso o configuración |
| `doctor` | ningún check falló | un check falló | no es un repositorio |

Ambos aceptan `-json` y emiten un schema versionado (`vector.audit/v1`, `vector.doctor/v1`). **Gateá por el campo JSON, nunca por la presencia de salida.**

---

## Estado

Funcionando y usado sobre sí mismo: 11 comandos, 247 tests, cero llamadas a modelos, dos dependencias.

Claude Code es el único agente cuyos hooks escribe `init` hoy. Codex, Cursor y Gemini exponen el mismo primitivo con otros nombres de evento, así que los adapters son traducción y no arquitectura nueva — pero no están escritos, y `doctor` va a reportar T2 honestamente en esos.

Sigue abierto: los adapters de hooks de Codex, Cursor y Gemini. Exponen el mismo primitivo con otros nombres de evento, así que es traducción y no arquitectura nueva — pero hasta que estén escritos, `doctor` reporta T2 en esas máquinas y lo dice en serio.

El número que justificaría todo esto sigue sin existir: nadie midió cuán seguido un agente se va realmente de alcance en estos repositorios. `vector audit` es el instrumento, y correrlo una semana no cuesta nada.

---

## Referencia

[`docs/reference.md`](docs/reference.md) es la referencia completa: qué es requerido
y qué apenas recomendado, cada comando con sus flags y códigos de salida, cada
archivo que vector escribe y si va o no a git, y qué nivel de enforcement se alcanza
realmente en cada agente. Está en inglés, como el resto de los artefactos técnicos.

## Desarrollo

```bash
go test ./...        # 247 tests
go vet ./...
gofmt -l .
```

Esos tres corren en cada push, si habilitás el hook una vez por clon:

```bash
git config core.hooksPath .githooks
```

Rechaza el push y te dice cuál de los tres falló, imprimiendo solo la parte que
falló. `git push --no-verify` lo saltea — un chequeo que no podés evadir cuando
sabés más que él es un chequeo que la gente deja de usar.

No hay workflow de CI. Los chequeos que correría un pipeline pasan antes del
push, que es más rápido de accionar y no depende de que una cuenta esté al día.
El workflow de release se queda, porque compilar binarios no es algo que
convenga confiarle a una laptop de forma reproducible.

Dos dependencias: [`BurntSushi/toml`](https://github.com/BurntSushi/toml) y [`bmatcuk/doublestar`](https://github.com/bmatcuk/doublestar) — esta última porque el `filepath.Match` de la biblioteca estándar no soporta `**`.

---

<div align="center">

[English](README.md) · **Español**

</div>
