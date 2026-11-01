/* AGENTTOWER-DEEPSEEK-HARNESS-PLUGIN
 * Cordis plugin for DeepSeek Harness. Loaded with `dsh web --patch`.
 * The AgentTower connector is the only network relay; this plugin uses loopback.
 */
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export const name = 'agenttower-deepseek-harness';
export const inject = ['agents'];

const agentName = 'DeepSeek Harness';
const instanceId = `dsh:${os.hostname()}:${process.pid}:${crypto.randomUUID()}`;
const contextId = instanceId;
const port = Number(process.env.AGENTTOWER_LOCAL_PORT || 17654);
const address = `ws://127.0.0.1:${port}`;

function userMessage(value) {
  return { id: crypto.randomUUID(), role: 'user', source: { kind: 'user' },
    content: [{ type: 'text', text: value }] };
}

function textContent(message) {
  return (message?.content || []).filter(part => part?.type === 'text')
    .map(part => part.text || '').join('\n\n').trim().slice(0, 12000);
}

function eventText(event) {
  if (event.type === 'user/message') return textContent(event.data);
  if (event.type === 'assistant/message') return textContent(event.data?.message);
  return '';
}

function recentEvents(session, count = 400) {
  const end = Number(session.seq || 0);
  const events = [];
  for (let i = Math.max(0, end - count); i < end; i++) {
    const event = session.eventAt(i);
    if (event) events.push(event);
  }
  return events;
}

export function apply(ctx) {
  const sessions = new Map();
  const pending = new Map();
  const toolCalls = new Map();
  let socket;
  let reconnect;
  let closed = false;

  const send = message => {
    if (socket?.readyState !== WebSocket.OPEN) return false;
    try {
      socket.send(JSON.stringify({ ...message, instanceId, contextId }));
      return true;
    } catch { return false; }
  };
  const hello = () => send({
    type: 'monitor.hello', connection: 'control',
    instance: { id: instanceId, agent: agentName, hostname: os.hostname(), pid: process.pid,
      directory: process.cwd(), contextId, capabilities: { sessionControl: true, permission: true } },
  });
  const snapshot = () => send({ type: 'monitor.snapshot',
    sessions: [...sessions.values()].filter(s => !s.parentID),
    permissions: [...pending.values()].map(p => p.request),
  });
  const publish = (agent, override = {}) => {
    const id = String(agent.id);
    const previous = sessions.get(id) || {};
    const session = {
      ...previous,
      id, agent: agentName, contextId,
      directory: agent.session.header.cwd || process.cwd(),
      parentID: agent.session.header.parentSession || null,
      title: previous.title || id.slice(0, 12),
      status: agent.status === 'running' ? 'busy' : 'idle',
      createdAt: agent.session.header.createdAt,
      updatedAt: Date.now(), ...override,
    };
    sessions.set(id, session);
    if (!session.parentID) send({ type: 'monitor.session', session });
  };
  const actionResult = (message, ok, extra = {}) => send({
    type: 'monitor.action.result', actionID: message.actionID, action: message.action,
    sessionID: message.sessionID || extra.sessionID, ok, ...extra,
  });

  const modelSelection = model => {
    if (model) {
      const at = model.indexOf('/');
      if (at <= 0 || at === model.length - 1) throw new Error('模型格式应为 provider/model');
      return { provider: model.slice(0, at), model: model.slice(at + 1) };
    }
    const selected = ctx.get('agentDefaultModel')?.currentSelection?.();
    if (selected?.provider && selected?.model) return { provider: selected.provider, model: selected.model };
    const provider = process.env.AGENTTOWER_DSH_PROVIDER;
    const defaultModel = process.env.AGENTTOWER_DSH_MODEL;
    if (provider && defaultModel) return { provider, model: defaultModel };
    const existing = ctx.agents.roots().find(agent => agent.options?.provider && agent.options?.model);
    if (existing) return { provider: existing.options.provider, model: existing.options.model };
    throw new Error('未找到默认模型；请在 Harness 配置模型或设置 AGENTTOWER_DSH_PROVIDER / AGENTTOWER_DSH_MODEL');
  };

  async function control(message) {
    try {
      if (message.contextId !== contextId) throw new Error('项目路由已失效');
      if (message.action === 'models') {
        const selection = modelSelection();
        actionResult(message, true, { models: [{ id: `${selection.provider}/${selection.model}`, name: selection.model, provider: selection.provider }] });
        return;
      }
      if (message.action === 'create') {
        const directory = message.directory || process.cwd();
        if (!path.isAbsolute(directory) || !fs.statSync(directory).isDirectory()) throw new Error('请选择此电脑上存在的项目目录');
        const options = modelSelection(message.model);
        const id = crypto.randomUUID();
        const handle = await ctx.agents.create({ sessionId: id, meta: { cwd: directory }, agentOptions: options });
        publish(handle.agent, { title: String(message.title || '').slice(0, 200) || id.slice(0, 12) });
        if (message.text?.trim()) handle.agent.followup(userMessage(message.text.trim()));
        actionResult(message, true, { sessionID: id });
        return;
      }
      const agent = ctx.agents.get(message.sessionID);
      if (!agent || !sessions.has(message.sessionID)) throw new Error('会话已失效');
      if (message.action === 'prompt') {
        if (!message.text?.trim()) throw new Error('消息不能为空');
        if (message.model) throw new Error('当前适配器不支持为已有会话切换模型');
        agent.followup(userMessage(message.text.trim()));
        actionResult(message, true);
      } else if (message.action === 'abort') {
        agent.cancel({ kind: 'user' });
        actionResult(message, true);
      } else if (message.action === 'history') {
        const messages = recentEvents(agent.session, 2000).filter(e => e.type === 'user/message' || e.type === 'assistant/message')
          .map(e => ({ messageID: String(e.seq), role: e.type === 'user/message' ? 'user' : 'assistant', text: eventText(e), at: e.time }))
          .filter(e => e.text).slice(-50);
        actionResult(message, true, { messages });
      } else throw new Error(`不支持的会话操作: ${message.action}`);
    } catch (error) {
      actionResult(message, false, { error: String(error?.message || error) });
    }
  }

  function permissionReply(message) {
    const item = pending.get(message.requestID);
    if (!item || item.sessionID !== message.sessionID && message.sessionID) return;
    pending.delete(message.requestID);
    send({ type: 'monitor.permission.resolved', requestID: message.requestID, reply: message.reply });
    item.resolve(message.reply === 'once' ? 'allowed-once' : 'rejected');
  }

  function connect() {
    if (closed) return;
    try {
      socket = new WebSocket(address);
      socket.addEventListener('open', () => { hello(); snapshot(); });
      socket.addEventListener('message', event => {
        try {
          const message = JSON.parse(String(event.data));
          if (message.instanceId && message.instanceId !== instanceId) return;
          if (message.type === 'control.session') void control(message);
          if (message.type === 'control.permission.reply') permissionReply(message);
        } catch (error) { ctx.logger?.warn?.(`AgentTower 控制消息无效: ${error}`); }
      });
      socket.addEventListener('close', disconnected);
      socket.addEventListener('error', disconnected);
    } catch { disconnected(); }
  }
  function disconnected() {
    if (closed) return;
    for (const item of pending.values()) item.resolve('unavailable');
    pending.clear();
    if (!reconnect) reconnect = setTimeout(() => { reconnect = null; connect(); }, 1000);
    reconnect.unref?.();
  }

  for (const agent of ctx.agents.list()) publish(agent);
  ctx.on('agent/created', ({ agent }) => { publish(agent); });
  ctx.on('agent/status', ({ agent, status }) => { publish(agent, { status: status === 'running' ? 'busy' : 'idle' }); });
  ctx.on('agent/disposed', ({ agent }) => {
    const id = String(agent.id);
    sessions.delete(id);
    for (const key of toolCalls.keys()) if (key.startsWith(`${id}:`)) toolCalls.delete(key);
    send({ type: 'monitor.session.remove', sessionID: id });
  });
  ctx.on('session/event', (session, event) => {
    const id = String(session.id);
    if (!sessions.has(id)) return;
    if (event.type === 'tool/call') toolCalls.set(`${id}:${event.data.callId}`, event.data);
    if (event.type === 'tool/result') toolCalls.delete(`${id}:${event.data.message?.toolCallId}`);
    if (event.type === 'user/message' || event.type === 'assistant/message') {
      const text = eventText(event);
      if (text) send({ type: event.type === 'user/message' ? 'monitor.input' : 'monitor.output',
        sessionID: id, messageID: String(event.seq), text });
    }
    if (event.type === 'turn/end' && event.data.reason?.kind === 'error') {
      const agent = ctx.agents.get(id);
      if (agent) publish(agent, { status: 'error' });
    }
  });
  ctx.on('approval/request', (request, next) => {
    if (!sessions.has(String(request.agent.id)) || socket?.readyState !== WebSocket.OPEN) return next();
    const id = crypto.randomUUID();
    const sessionID = String(request.agent.id);
    const call = toolCalls.get(`${sessionID}:${request.callId}`);
    const detail = call?.arguments ? (typeof call.arguments === 'string' ? call.arguments : JSON.stringify(call.arguments)) : String(request.reason || request.toolName);
    return new Promise(resolve => {
      const timer = setTimeout(() => settle('unavailable'), 110000);
      const settle = outcome => {
        clearTimeout(timer);
        request.signal?.removeEventListener('abort', aborted);
        if (pending.delete(id)) send({ type: 'monitor.permission.resolved', requestID: id });
        resolve(outcome);
      };
      const aborted = () => settle('cancelled');
      const entry = { sessionID, resolve: settle,
        request: { id, sessionID, directory: request.agent.session.header.cwd || process.cwd(), permission: request.toolName, patterns: [detail], allowSimilar: !!call?.arguments,
          metadata: request.reason || '', status: 'pending', createdAt: Date.now() } };
      pending.set(id, entry);
      request.signal?.addEventListener('abort', aborted, { once: true });
      if (request.signal?.aborted) settle('cancelled');
      else if (!send({ type: 'monitor.permission', request: entry.request })) settle('unavailable');
    });
  }, { prepend: true });

  ctx.effect(() => {
    connect();
    return () => {
      closed = true;
      clearTimeout(reconnect);
      for (const item of pending.values()) item.resolve('unavailable');
      pending.clear();
      socket?.close();
    };
  });
}
