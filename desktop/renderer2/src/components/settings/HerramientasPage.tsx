// Ajustes · Herramientas (Settings rework, mock .dev/mocks/settings-v2.html
// page 5). What Workass gives agents on this machine beyond their own CLI, one
// switch per group, each with what it costs in context; and, read-only, the MCP
// servers each agent's own CLI is configured with. Every number comes from the
// daemon (agent-tools:get), counted from the text agents are actually given.

import { useEffect, useState } from 'react';
import { useApp } from '../../store/store';
import { call, callThrow, has } from '../../wire/api';
import type { AgentMcpServer, AgentMcpServers, AgentToolsView } from '../../wire/types';
import { fmtTokens, mcpServerDetail, mcpStatusLabel, mcpEmptyLine } from './herramientas';

// Group names and one line each. An id this renderer does not know still gets
// its row, under its id, so a newer daemon's group can be switched.
const GROUPS: Record<string, { n: string; d: string }> = {
  browser: { n: 'Navegador', d: 'Abrir páginas en varias pestañas, con tus sesiones. Invisible para vos.' },
  pages: { n: 'Páginas en el chat', d: 'Mostrar HTML dentro del chat: mocks, gráficos, reportes.' },
  subagents: { n: 'Subagentes', d: 'Lanzar otros agentes, de cualquier proveedor y máquina, uno o varios a la vez.' },
  jobs: { n: 'Trabajos', d: 'Ver y cancelar trabajos largos en segundo plano, y lanzarlos en otra máquina.' },
  scheduled: { n: 'Mensajes programados', d: 'Escribirle a un chat a una hora, o cuando pase algo.' },
  chats: { n: 'Otros chats', d: 'Listar, leer, crear y escribirle a otros chats.' },
  machines: { n: 'Máquinas y actualizaciones', d: 'Ver tus máquinas, descubrir nuevas, instalar una versión cuando se lo pedís.' },
};

function ToolGroups() {
  const [view, setView] = useState<AgentToolsView | null>(null);
  const [saving, setSaving] = useState('');
  useEffect(() => {
    let live = true;
    void call('agentToolsGet').then((v) => { if (live && v) setView(v); });
    return () => { live = false; };
  }, []);
  if (!has('agentToolsGet')) return null;
  if (!view) return null;

  const toggle = async (id: string, on: boolean) => {
    if (saving) return;
    setSaving(id);
    try {
      const next = await callThrow('agentToolsSet', id, on);
      if (next) setView(next);
    } catch (err) {
      console.warn('[settings] agentToolsSet failed', err);
    } finally {
      setSaving('');
    }
  };
  const canSet = has('agentToolsSet');

  return (
    <>
      <div className="gtitle">Herramientas de Workass</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">Lo que Workass suma a cada agente</div>
            <div className="d">{`La guía ${fmtTokens(view.guideTokens)} · la lista de herramientas ${fmtTokens(view.listTokens)}`}</div>
          </div>
          <div className="ctl"><span className="stat"><b style={{ color: 'var(--ink)' }}>{fmtTokens(view.totalTokens)}</b> tokens</span></div>
        </div>
        {view.groups.map((g) => {
          const label = GROUPS[g.id] ?? { n: g.id, d: '' };
          return (
            <div className="set" key={g.id}>
              <div className="lbl"><div className="n">{label.n}</div>{label.d && <div className="d">{label.d}</div>}</div>
              <div className="ctl">
                <span className="stat mono" title={`${g.tools} ${g.tools === 1 ? 'herramienta' : 'herramientas'}`}>{fmtTokens(g.tokens)}</span>
                {canSet && (
                  <button className={`toggle ${g.on ? 'on' : ''}`} role="switch" aria-checked={g.on}
                    aria-label={`${g.on ? 'Apagar' : 'Prender'} ${label.n}`} disabled={!!saving} aria-busy={saving === g.id}
                    onClick={() => void toggle(g.id, !g.on)}>
                    <span className="knob" />
                  </button>
                )}
              </div>
            </div>
          );
        })}
        <div className="set">
          <div className="lbl">
            <div className="n">Siempre activas</div>
            <div className="d">Preguntarte algo, entregarte archivos, ver los modelos y avisar de cada <span className="mono">workass run</span>.</div>
          </div>
          <div className="ctl"><span className="stat mono">{fmtTokens(view.essential.tokens)}</span></div>
        </div>
      </div>
    </>
  );
}

function McpServers() {
  const app = useApp();
  const [agents, setAgents] = useState<AgentMcpServers[] | null>(null);
  useEffect(() => {
    let live = true;
    void call('agentMcpList').then((r) => { if (live && r && Array.isArray(r.agents)) setAgents(r.agents); });
    return () => { live = false; };
  }, []);
  if (!has('agentMcpList') || !agents) return null;
  const rows: Array<{ key: string; agent: AgentMcpServers; server?: AgentMcpServer }> = [];
  for (const agent of agents) {
    if (agent.error) rows.push({ key: `${agent.providerId}:error`, agent });
    for (const server of agent.servers) rows.push({ key: `${agent.providerId}:${server.scope}:${server.project ?? ''}:${server.name}`, agent, server });
  }
  return (
    <>
      <div className="gtitle">Servidores MCP de cada agente <span className="gtag">solo lectura · se configuran en cada CLI</span></div>
      <div className="group">
        {agents.length > 0 && rows.length === 0 && (
          <div className="set"><div className="lbl"><div className="d">{mcpEmptyLine(agents.map((a) => a.name))}</div></div></div>
        )}
        {rows.map(({ key, agent, server }) => {
          if (!server) {
            return (
              <div className="set" key={key}>
                <div className="lbl"><div className="n">{agent.name}</div><div className="d">{`No se pudo leer su configuración: ${agent.error}`}</div></div>
              </div>
            );
          }
          const status = mcpStatusLabel(server.status);
          return (
            <div className="set" key={key}>
              <div className="lbl">
                <div className="n">{`${agent.name} · ${server.name}`}</div>
                <div className="d">{mcpServerDetail(server, app.workspaces)}</div>
              </div>
              {status && <div className="ctl"><span className="stat"><span className={`dot ${status.tone}`} />{status.text}</span></div>}
            </div>
          );
        })}
      </div>
    </>
  );
}

export function HerramientasPage() {
  return (
    <section className="stgs-panel">
      <ToolGroups />
      <McpServers />
    </section>
  );
}
