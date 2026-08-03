# Composer skills/agents popup — notas del mock

Mock: `mock.html` (estados A–D, toggle claro/oscuro arriba a la derecha).
Fuente de datos: el catálogo que el Agent SDK reporta al inicio de sesión
(slash-commands, subagent types, output styles) y que hoy el renderer ya
recibe parcialmente (`chat.commands` en `wire/types.ts`); agentes y estilos
son campos nuevos del mismo canal. Todo es catálogo por-chat, nunca global.

## Placement

- El popup vive dentro de `.compwrap`, como el `slashpop` actual: bloque
  arriba de `.comp`, mismo ancho (la medida de lectura `--read`), separado
  8px, borde `--line2`, radio 10, sombra suave. Crece hacia arriba; la lista
  interna (`.slist`) scrollea a partir de 240px, el header de pestañas y el
  pie de atajos quedan fijos.
- Header: pestañas Comandos / Agentes / Estilos. Pestaña activa: tinta sobre
  `--field`. Una categoría vacía no se renderiza (sin pestañas muertas).
  Pie: hints de teclado (`↑↓ navegar · ⏎ usar · ⇥ completar · esc cerrar`).
- Fila: nombre mono en tinta con el sigilo (`/` o `@`) en `--faint`,
  descripción en `--muted` con ellipsis, y en Agentes el modelo del subagente
  a la derecha (mono, `--faint`, clamp 12ch). La fila activa lleva fondo
  `--line` y el badge `⏎`. Los caracteres matcheados del fuzzy son el ÚNICO
  verde del popup (`--acc`, weight 600) — cambio deliberado vs. el slashpop
  actual, que pintaba el nombre entero de verde.
- Clamps: nombre de comando/agente `max-width: 46%` con ellipsis; descripción
  1 línea; chip de modelo 12ch. Nada envuelve, nada empuja la fila.

## Modelo de interacción

Disparadores (sobre el draft, no sobre el transcript):

- `/` como primer carácter del draft (sin newline todavía) abre la pestaña
  **Comandos**. Query = lo tecleado después de `/` hasta el primer espacio.
  Después de elegir (o de completar un nombre exacto + espacio) el popup se
  cierra y lo que sigue son argumentos del comando, texto libre.
- `@` en límite de palabra (inicio o después de espacio) abre **Agentes**,
  estilo mención, en cualquier punto del mensaje. Query = lo que sigue al `@`.
- Las pestañas se clickean para saltar de catálogo sin perder lo tecleado;
  Estilos usa la misma mecánica (elegir un estilo no inserta texto: fija el
  output style de la sesión y se muestra donde el chat ya muestra su modo).

Teclado con popup abierto:

- `↑`/`↓` mueven la fila activa (con wrap); la lista scrollea para seguirla.
- `⏎` elige la activa: inserta `/nombre␣` (o `@nombre␣`) en el caret y
  cierra. **Nunca envía.** `⏎` solo envía con el popup cerrado.
- `⇥` completa al nombre de la fila activa (igual que `⏎`; hoy Tab ya
  completa la primera sugerencia — pasa a completar la activa).
- `esc` cierra y deja el texto literal tal cual. Latch por token: el popup no
  reaparece hasta que ese token cambie (borrar/retipear).
- `⇧⏎` siempre es newline; un draft multilínea no dispara popups en líneas
  siguientes que empiecen con `/` (solo la primera posición del draft).
- Con turno corriendo no cambia nada: elegir inserta texto y el envío sigue
  las reglas de steer/queue existentes del composer.

Filtrado: subsecuencia case-insensitive; ranking prefijo > límite de palabra
(`/re` encuentra `probar-remoto`) > dispersa. Sin resultados → el popup se
cierra solo (cero cascarón vacío). Lista con scroll; no se trunca a 8 como el
slice actual.

Qué escribe cada elección:

- Comando → texto literal `/deploy ` + argumentos del usuario; se envía como
  prompt normal y el SDK ejecuta el slash-command.
- Agente → mención `@revisor-de-pr `; el resto del mensaje es la tarea. El
  host la resuelve al subagent type reportado (no inventamos sintaxis nueva:
  es la mención que Claude Code ya entiende).

## Degradación con catálogo vacío

- Provider sin comandos (mock, provider viejo, sesión aún fría): `/` y `@`
  son texto literal. No hay popup, ni estado "Sin comandos", ni toast. El
  composer queda idéntico al de hoy (estado D del mock).
- Por categoría: hay comandos pero no agentes → la pestaña Agentes no existe
  y `@` queda inerte (y viceversa). Estilos igual.
- Catálogo tardío (llega con el usuario ya tipeando): el popup puede aparecer
  recién en el próximo keystroke; jamás roba foco ni se come la tecla en
  vuelo. Si el usuario ya cerró con `esc`, el latch se respeta.
- Catálogo que se vacía con el popup abierto (reconexión): se cierra en
  silencio.

## Accesibilidad

Popup `role="listbox"` con `aria-label` ("Comandos del agente" /
"Subagentes disponibles"), filas `role="option"` + `aria-selected`; el
textarea apunta con `aria-activedescendant` y anuncia
`aria-expanded`/`aria-controls`. Los hints del pie son texto real, no solo
iconografía. Ambos temas mantienen AA con los tokens aprobados.
