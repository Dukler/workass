# Estado del turno — notas de los mocks (A y B)

Motivación (incidente 2026-07-27): un turno de Claude pensando 3 minutos en `max` es indistinguible
de un chat muerto; los reintentos 529 son invisibles; un chat puede correr en el modelo equivocado
sin decirlo. Tres piezas: latido del turno, chip de reintento, aviso de desajuste de modelo.
Ambos mocks laten en vivo (elapsed corre siempre; tokens sólo cuando llega stream — congelarse
durante un reintento es parte de la honestidad) y traen controles de demo para ver aparecer/desaparecer
cada estado. Nada de spinners que mientan: todo movimiento está atado a datos reales.

## Dirección A — «Pulso en línea» (todo es hilo)

**Racional.** Cero chrome nuevo: las tres piezas son filas del transcript, en el idioma ya aprobado.
El latido es la fila viva `.thinklive` existente más un clúster de vitales mono a la derecha
(`1:47 · 2,3k tokens`); el chip 529 se inserta *dentro* de esa fila (un reintento sólo existe durante
un turno vivo, así que siempre tiene dónde vivir) y la fase se atenúa mientras dura; el desajuste es
una fila-recibo con borde ámbar en la cabeza del turno afectado, que persiste en la historia como
cualquier otro evento. Al terminar el turno, las vitales se funden en el `.stamp`
(`hace un momento · 3m 12s · 4,1k tokens`): un renglón tenue, cero ruido nuevo.

**Ubicación exacta en el timeline real.**
- Latido: extiende la fila `.thinklive` que `Transcript.tsx` ya renderiza como cola sticky del turno
  corriendo (app.css:446). Se agrega el clúster `.vitals` con `margin-left:auto`; nada se mueve.
- Chip de reintento: hijo de esa misma fila (`.liverow .rchip`), montado sólo mientras el bridge
  reporta retry; al recuperarse se desmonta y la fase vuelve a plena opacidad.
- Desajuste: primera fila del turno (antes del primer token del assistant), renderizada como evento
  del hilo con la receta de card (`--card` + borde `--line` + filo izquierdo `--amber`). «Reintentar»
  relanza el turno con el modelo configurado; la fila queda como recibo.

## Dirección B — «Franja de estado» (chrome dedicado)

**Racional.** La salud del turno tiene una sola casa, visible aunque scrollees a mensajes viejos:
una franja anclada entre `.scroll` y `.compwrap` (el patrón `contbar` del mock de continuación
aprobado). Lleva EKG sutil en `--acc`, fase + nota clampada, y vitales mono; el reintento es un
*estado* de la misma franja (borde ambarizado, EKG apagado, `reintentando (529) · intento 2 ·
próximo en 8 s` — la cuenta regresiva es opcional, sólo si el daemon expone el backoff). El
desajuste es un banner anclado bajo la `.tbar`, ocultable con ✕ porque el recibo permanente es una
línea tenue en el hilo (ocultar no borra historia) y el selector de modelo del compositor gana un
punto ámbar con title, atando el aviso al lugar donde se elige el modelo. Al terminar el turno la
franja se pliega (grid-rows → 0) y no queda chrome.

**Ubicación exacta en el timeline real.**
- Franja: hermana de `.scroll`, arriba de `.compwrap`, dentro de `main` (App.tsx); ancho `--read`
  centrado, alineada con el compositor. Montada sólo mientras `message.status === 'running'`.
- Banner: entre `.tbar` y `.scroll`, mismo contenedor; línea-recibo `.mmline` en el hilo en la
  cabeza del turno afectado; punto `.mdot` en el selector de modelo de `.comprow`.

## Variables y clases de renderer2 espejadas

- `tokens.css` copiado **verbatim** (dark + light): `--bg --side --panel --card --field --ink --ink2
  --muted --faint --line --line2 --acc --link --amber --add --del --shell`. Tipografía del body
  idéntica (13.5px/1.6 -apple-system) y monos en `ui-monospace, Menlo` a 11px como `.tg-dur/.evt-st`.
- Clases/recetas reutilizadas: `.thinklive` + `.ping` + `.stword`/`stwordin` (fila viva aprobada),
  `.steprow` (pliegue «Pensó durante…»), `.stamp`, `.userpill/.userpill-text`, `.p code`, el idioma
  de `.tg-summary` (subrayado punteado + duración mono) en `.toolline`, `.permchip` (ámbar 12%),
  `.comp/.comprow`, la receta `.uretry` para «Reintentar» (pasada a ámbar), y el colapso
  `grid-template-rows` del updcard para franja/banner.
- Leyes: verde `--acc` sólo en latido (ping/EKG) — las vitales son `--muted`, nunca verdes; ámbar
  `--amber` para advertencias (idioma existente de `.permchip`/`.steerstate.uncertain`); AA en ambos
  temas (texto informativo ≥ `--muted`; `--faint` sólo separadores/metadata terciaria); nombres
  largos clampan (`.ltool/.tb-tool/.mmtext` con title); iconos literales (triángulo=advertencia,
  flechas circulares=reintento, EKG=latido — el mismo glifo del rail «Sin turno activo»);
  `prefers-reduced-motion` apaga ping/EKG/blur como en app.css.
