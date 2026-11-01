/**
 * AGENT-TRAFFIC-LIGHT-OPENCODE-PLUGIN
 * OpenCode Plugin -> Agent Traffic Light bridge.
 *
 * Normal events use short-lived WebSocket connections. One lightweight
 * persistent connection receives permission replies and session controls.
 *
 * Environment:
 *   AGENT_LIGHT_URL  Widget WebSocket URL (default: ws://127.0.0.1:17654)
 */

import fs from 'fs';
import os from 'os';
import path from 'path';

const AGENT = 'opencode';
const WIDGET_URL = process.env.AGENT_LIGHT_URL || 'ws://127.0.0.1:17654';

async function storedProjectDirectories() {
  const file = path.join(process.env.XDG_DATA_HOME || path.join(os.homedir(), '.local', 'share'), 'opencode', 'opencode.db');
  if (!fs.existsSync(file)) return [];
  let db;
  try {
    if (typeof Bun !== 'undefined') {
      const { Database } = await import('bun:sqlite');
      db = new Database(file, { readonly: true });
    } else {
      const { DatabaseSync } = await import('node:sqlite');
      db = new DatabaseSync(file, { readOnly: true });
    }
    const query = (sql) => typeof db.query === 'function' ? db.query(sql).all() : db.prepare(sql).all();
    const directories = [];
    // Non-git projects share worktree="/"; their real folders are in sessions.
    for (const sql of ['SELECT worktree AS directory FROM project', 'SELECT DISTINCT directory FROM session']) {
      try { directories.push(...query(sql).map((row) => row.directory)); } catch {}
    }
    return directories;
  } catch (error) {
    log(`project history unavailable: ${error.message}`);
    return [];
  } finally { try { db?.close(); } catch {} }
}

function existingProjectDirectories(directories) {
  const unique = new Map();
  for (const directory of directories) {
    if (typeof directory !== 'string' || !path.isAbsolute(directory)) continue;
    const normalized = path.normalize(directory);
    if (normalized === path.parse(normalized).root) continue;
    try {
      if (fs.statSync(normalized).isDirectory()) unique.set(process.platform === 'win32' ? normalized.toLowerCase() : normalized, normalized);
    } catch {}
  }
  return [...unique.values()];
}

function stateDir() {
  const home = os.homedir();
  if (process.platform === 'win32' && process.env.APPDATA) {
    return path.join(process.env.APPDATA, 'agent-traffic-light', 'hooks-bridge', 'state');
  }
  if (process.platform === 'darwin') {
    return path.join(home, 'Library', 'Application Support', 'agent-traffic-light', 'hooks-bridge', 'state');
  }
  return path.join(home, '.config', 'agent-traffic-light', 'hooks-bridge', 'state');
}

function log(msg) {
  try {
    const dir = stateDir();
    fs.mkdirSync(dir, { recursive: true });
    const ts = new Date().toISOString().replace('T', ' ').slice(0, 19);
    fs.appendFileSync(path.join(dir, `${AGENT}-hooks.log`), `[${ts}] [${AGENT}] ${msg}\n`);
  } catch {}
}

function safeValue(value, depth = 0) {
  if (depth > 3) return '[truncated]';
  if (typeof value === 'string') return value.length > 2000 ? `${value.slice(0, 2000)}...` : value;
  if (Array.isArray(value)) return value.slice(0, 20).map((item) => safeValue(item, depth + 1));
  if (!value || typeof value !== 'object') return value;
  const result = {};
  for (const [key, item] of Object.entries(value).slice(0, 30)) {
    result[key] = /token|secret|password|authorization|api.?key/i.test(key)
      ? '[redacted]'
      : safeValue(item, depth + 1);
  }
  return result;
}

export const OpenCodeBridge = async ({ client, directory, worktree, serverUrl }) => {
  // A server can load this module for more than one project context.
  const INSTANCE_ID = `opencode:${os.hostname()}:${process.pid}:${Date.now()}:${Math.random().toString(36).slice(2)}`;
  const startedAt = Date.now();
  const contextId = `${INSTANCE_ID}:${worktree || directory || serverUrl}`;
  const sessions = new Map();
  const permissions = new Map();
  const questions = new Map();
  const assistantMessages = new Map();
  const userMessages = new Map();
  const messageParts = new Map();
  const lastOutputs = new Map();
  const seenUserMessages = new Set();
  let lastMode = '';
  let controlWs = null;
  let reconnectTimer = null;
  let pushQueue = Promise.resolve();
  let projects = existingProjectDirectories([directory, ...(await storedProjectDirectories())]);

  function hello(connection = 'transient') {
    return {
      type: 'monitor.hello',
      connection,
      instanceId: INSTANCE_ID,
      contextId,
      instance: {
        id: INSTANCE_ID,
        agent: AGENT,
        hostname: os.hostname(),
        pid: process.pid,
        directory,
        worktree,
        startedAt,
        contextId,
        projects,
        capabilities: { sessionControl: true },
      },
    };
  }

  function pushOne(message) {
    return new Promise((resolve) => {
      let settled = false;
      let ws;
      const done = () => {
        if (settled) return;
        settled = true;
        try { ws && ws.close(); } catch {}
        resolve();
      };
      try {
        ws = new WebSocket(WIDGET_URL);
        ws.addEventListener('open', () => {
          try {
            ws.send(JSON.stringify(hello()));
            ws.send(JSON.stringify({ ...message, instanceId: INSTANCE_ID, contextId }));
            // Bun/browser WebSocket.send has no completion callback.
            setTimeout(done, 30);
          } catch {}
        });
        ws.addEventListener('error', done);
        ws.addEventListener('close', done);
        setTimeout(done, 3000);
      } catch {
        done();
      }
    });
  }

  function push(message) {
    if (controlWs?.readyState === 1) {
      sendControl(message);
      return Promise.resolve();
    }
    pushQueue = pushQueue.then(() => pushOne(message), () => pushOne(message));
    return pushQueue;
  }

  function sendStatus(mode) {
    const changed = mode !== lastMode;
    lastMode = mode;
    push({ type: 'status', value: mode, agent: AGENT });
    if (changed) log(`status -> ${mode}`);
  }

  function sendSnapshot(target = push) {
    target({
      type: 'monitor.snapshot',
      sessions: Array.from(sessions.values()).filter((session) => !session.parentID),
      permissions: Array.from(permissions.values()),
    });
    log(`snapshot sessions=${sessions.size} permissions=${permissions.size}`);
  }

  function sendControl(message) {
    if (!controlWs || controlWs.readyState !== 1) return;
    try { controlWs.send(JSON.stringify({ ...message, instanceId: INSTANCE_ID, contextId })); } catch {}
  }

  function closeControlIfIdle() {
    // Session controls must remain available while OpenCode is idle.
  }

  function scheduleControlReconnect() {
    if (reconnectTimer) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      ensureControlConnection();
    }, 1000);
    reconnectTimer.unref?.();
  }

  function ensureControlConnection() {
    if (controlWs && (controlWs.readyState === 0 || controlWs.readyState === 1)) return;
    try {
      const ws = new WebSocket(WIDGET_URL);
      controlWs = ws;
      const disconnected = () => {
        if (controlWs !== ws) return;
        controlWs = null;
        scheduleControlReconnect();
      };
      ws.addEventListener('open', () => {
        try {
          ws.send(JSON.stringify(hello('control')));
          sendSnapshot(sendControl);
          void loadInitialState();
        } catch {}
      });
      controlWs.addEventListener('message', (event) => {
        try {
          const message = JSON.parse(event.data.toString());
          if (message.type === 'control.permission.reply') replyPermission(message);
          if (message.type === 'control.session') controlSession(message);
        } catch (e) {
          log(`control message error: ${e.message}`);
        }
      });
      ws.addEventListener('close', disconnected);
      ws.addEventListener('error', () => {
        disconnected();
        try { ws.close(); } catch {}
      });
    } catch {
      scheduleControlReconnect();
    }
  }

  async function api(pathname, init = {}, projectDirectory = directory) {
    const url = new URL(pathname, serverUrl);
    if (projectDirectory) url.searchParams.set('directory', projectDirectory);
    // Reuse OpenCode's injected transport: it carries server auth and may
    // dispatch in-process rather than through the HTTP listener.
    if (client._client?.request) {
      const result = await client._client.request({
        url: url.pathname + url.search,
        method: init.method || 'GET',
        headers: init.headers,
        body: init.body ? JSON.parse(init.body) : undefined,
        bodySerializer: JSON.stringify,
        responseStyle: 'fields',
        throwOnError: true,
        signal: AbortSignal.timeout(10000),
      });
      if (result.error) throw new Error(`OpenCode API ${pathname}: ${JSON.stringify(safeValue(result.error))}`);
      return result.data;
    }
    const headers = new Headers(init.headers);
    const password = process.env.OPENCODE_SERVER_PASSWORD;
    if (password) headers.set('authorization', `Basic ${Buffer.from(`${process.env.OPENCODE_SERVER_USERNAME || 'opencode'}:${password}`).toString('base64')}`);
    const response = await fetch(url, { ...init, headers, signal: AbortSignal.timeout(10000) });
    if (!response.ok) throw new Error(`HTTP ${response.status} ${pathname}`);
    if (response.status === 204) return null;
    return response.json();
  }

  async function controlSession(message) {
    const actionID = message.actionID;
    const action = message.action;
    try {
      if (action === 'create') {
        const projectDirectory = message.directory || directory;
        if (typeof projectDirectory !== 'string' || !path.isAbsolute(projectDirectory) || !fs.statSync(projectDirectory).isDirectory()) {
          throw new Error('请选择此电脑上存在的项目目录');
        }
        const title = typeof message.title === 'string' ? message.title.trim().slice(0, 200) : '';
        const session = await api('/session', {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify(title ? { title } : {}),
        }, projectDirectory);
        upsertSession({ ...session, directory: projectDirectory }, 'idle');
        projects = existingProjectDirectories([...projects, projectDirectory]);
        sendControl(hello('control'));
        const text = typeof message.text === 'string' ? message.text.trim() : '';
        if (text) {
          const modelValue = typeof message.model === 'string' ? message.model.trim() : '';
          const separator = modelValue.indexOf('/');
          const model = separator > 0 && separator < modelValue.length - 1
            ? { providerID: modelValue.slice(0, separator), modelID: modelValue.slice(separator + 1) }
            : undefined;
          await api(`/session/${encodeURIComponent(session.id)}/prompt_async`, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify({ ...(model ? { model } : {}), parts: [{ type: 'text', text }] }),
          }, projectDirectory);
          updateSession(session.id, { status: 'busy' });
        }
        sendSnapshot(sendControl);
        sendControl({ type: 'monitor.action.result', actionID, action, ok: true, sessionID: session.id });
      } else if (action === 'models') {
        const result = await api('/config/providers');
        if (!Array.isArray(result?.providers)) throw new Error('OpenCode 未返回可用提供商');
        const models = result.providers
          .flatMap((provider) => Object.entries(provider.models || {}).map(([modelID, model]) => ({
            id: `${provider.id}/${modelID}`,
            name: typeof model?.name === 'string' ? model.name : modelID,
            provider: typeof provider.name === 'string' ? provider.name : provider.id,
          })))
          .filter((model) => model.id.length <= 300);
        if (!models.length) throw new Error('当前 OpenCode 没有已配置的可用模型');
        sendControl({ type: 'monitor.action.result', actionID, action, ok: true, models });
      } else if (action === 'history') {
        if (!message.sessionID || !sessions.has(message.sessionID)) throw new Error('会话已失效');
        const records = await api(`/session/${encodeURIComponent(message.sessionID)}/message?limit=50`, {}, sessions.get(message.sessionID).directory || directory);
        if (!Array.isArray(records)) throw new Error('消息接口返回无效数据');
        const messages = records.flatMap((record) => {
          const info = record?.info;
          if (!info?.id || !['user', 'assistant'].includes(info.role)) return [];
          const text = (Array.isArray(record.parts) ? record.parts : [])
            .filter((part) => part?.type === 'text' && !part.synthetic && !part.ignored && typeof part.text === 'string')
            .map((part) => part.text).join('\n\n').trim().slice(0, 12000);
          if (!text) return [];
          return [{ messageID: info.id, role: info.role, text, at: info.time?.created || Date.now() }];
        }).sort((a, b) => a.at - b.at).slice(-50);
        sendControl({ type: 'monitor.action.result', actionID, action, ok: true, sessionID: message.sessionID, messages });
      } else if (action === 'abort') {
        if (!message.sessionID) throw new Error('缺少 sessionID');
        await api(`/session/${encodeURIComponent(message.sessionID)}/abort`, { method: 'POST' }, sessions.get(message.sessionID)?.directory || directory);
        updateSession(message.sessionID, { status: 'idle', tool: null });
        sendControl({ type: 'monitor.action.result', actionID, action, ok: true, sessionID: message.sessionID });
      } else if (action === 'prompt') {
        const text = typeof message.text === 'string' ? message.text.trim() : '';
        if (!message.sessionID) throw new Error('缺少 sessionID');
        if (!text) throw new Error('消息不能为空');
        const modelValue = typeof message.model === 'string' ? message.model.trim() : '';
        const separator = modelValue.indexOf('/');
        const model = separator > 0 && separator < modelValue.length - 1
          ? { providerID: modelValue.slice(0, separator), modelID: modelValue.slice(separator + 1) }
          : undefined;
        await api(`/session/${encodeURIComponent(message.sessionID)}/prompt_async`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ ...(model ? { model } : {}), parts: [{ type: 'text', text }] }),
        }, sessions.get(message.sessionID)?.directory || directory);
        updateSession(message.sessionID, { status: 'busy', error: null });
        sendControl({ type: 'monitor.action.result', actionID, action, ok: true, sessionID: message.sessionID });
      } else {
        throw new Error(`不支持的会话操作: ${action || '(empty)'}`);
      }
      log(`session control ${action} succeeded`);
    } catch (e) {
      const error = e?.message || (typeof e === 'string' ? e : JSON.stringify(safeValue(e)));
      sendControl({
        type: 'monitor.action.result', actionID, action, ok: false,
        sessionID: message.sessionID, error,
      });
      log(`session control ${action} failed: ${error}`);
    }
  }

  function normalizeSession(info, status) {
    return {
      id: info.id,
      parentID: info.parentID || null,
      title: info.title || 'Untitled session',
      directory: info.directory || directory,
      contextId,
      status: status || 'idle',
      createdAt: info.time?.created || Date.now(),
      updatedAt: info.time?.updated || Date.now(),
      summary: info.summary || null,
    };
  }

  function upsertSession(info, status) {
    if (!info?.id) return;
    const existing = sessions.get(info.id) || {};
    const session = { ...existing, ...normalizeSession(info, status || existing.status), parentID: info.parentID || existing.parentID || null };
    sessions.set(info.id, session);
    if (session.parentID) {
      if (existing.id && !existing.parentID) push({ type: 'monitor.session.remove', sessionID: info.id });
      return;
    }
    push({ type: 'monitor.session', session });
  }

  function updateSession(sessionID, patch) {
    if (!sessionID) return;
    const existing = sessions.get(sessionID) || {
      id: sessionID,
      parentID: null,
      title: sessionID.slice(0, 12),
      directory,
      createdAt: Date.now(),
    };
    const session = { ...existing, ...patch, contextId, updatedAt: Date.now() };
    sessions.set(sessionID, session);
    if (!session.parentID) push({ type: 'monitor.session', session });
  }

  function publishOutput(sessionID) {
    if (sessions.get(sessionID)?.parentID) return;
    const messageID = assistantMessages.get(sessionID);
    if (!messageID) return;
    const text = [...messageParts.values()]
      .filter((part) => part.sessionID === sessionID && part.messageID === messageID)
      .map((part) => part.text)
      .join('\n\n')
      .trim();
    if (!text || lastOutputs.get(sessionID) === text) return;
    lastOutputs.set(sessionID, text);
    push({ type: 'monitor.output', sessionID, messageID, text: text.slice(0, 12000) });
  }

  function publishInput(sessionID, messageID) {
    if (sessions.get(sessionID)?.parentID) return;
    if (!sessionID || !messageID || userMessages.get(messageID) !== sessionID) return;
    const text = [...messageParts.values()]
      .filter((part) => part.sessionID === sessionID && part.messageID === messageID)
      .map((part) => part.text)
      .join('\n\n').trim();
    if (text) push({ type: 'monitor.input', sessionID, messageID, text: text.slice(0, 12000) });
  }

  function upsertQuestion(item, projectDirectory = directory) {
    if (!item?.id || typeof item.sessionID !== 'string') return;
    const list = (Array.isArray(item.questions) ? item.questions : []).slice(0, 5).map((question) => ({
      header: String(question?.header || '').slice(0, 30),
      question: String(question?.question || '').slice(0, 2000),
      options: (Array.isArray(question?.options) ? question.options : []).slice(0, 20).map((option) => ({
        label: String(option?.label || '').slice(0, 200),
        description: String(option?.description || '').slice(0, 1000),
      })),
      multiple: question?.multiple === true,
      custom: question?.custom !== false,
    }));
    const first = list[0] || {};
    const known = permissions.has(item.id);
    questions.set(item.id, { items: list, directory: projectDirectory });
    const request = {
      id: item.id,
      sessionID: item.sessionID,
      permission: first.header || 'Agent 提问',
      patterns: [first.question || '请选择', ...(first.options || []).slice(0, 20).map((option, index) => `${index + 1}. ${option?.label ?? ''}`)],
      question: true,
      questions: list,
      status: 'pending',
      createdAt: Date.now(),
    };
    permissions.set(request.id, request);
    if (known) return;
    push({ type: 'monitor.permission', request });
    sendStatus('alarm');
  }

  async function loadInitialState() {
    const [sessionResult, statusResult, permissionResult, projectResult, questionResult] = await Promise.allSettled([
      api('/session?limit=100'),
      api('/session/status'),
      api('/permission'),
      api('/project'),
      api('/question'),
    ]);
    try {
      const sessionList = sessionResult.status === 'fulfilled' ? sessionResult.value : [];
      const statuses = statusResult.status === 'fulfilled' ? statusResult.value : {};
      const pending = permissionResult.status === 'fulfilled' ? permissionResult.value : [];
      const pendingQuestions = questionResult.status === 'fulfilled' && Array.isArray(questionResult.value) ? questionResult.value : [];
      if (projectResult.status === 'fulfilled' && Array.isArray(projectResult.value)) {
        projects = existingProjectDirectories([...projects, ...projectResult.value.flatMap((item) => [item.worktree, ...(item.sandboxes || [])]), ...(sessionList || []).map((item) => item.directory)]);
      }
      for (const info of sessionList || []) {
        sessions.set(info.id, normalizeSession(info, statuses[info.id]?.type || 'idle'));
      }
      for (const request of pending || []) {
        permissions.set(request.id, {
          ...safeValue(request),
          id: request.id,
          status: 'pending',
          createdAt: Date.now(),
        });
      }
      for (const item of pendingQuestions) upsertQuestion(item, directory);
      sendSnapshot();
      sendControl(hello('control'));
      sendSnapshot(sendControl);
      ensureControlConnection();
    } catch (e) {
      log(`initial state failed: ${e.message}`);
    }
  }

  async function replyPermission(message) {
    const request = permissions.get(message.requestID);
    if (!request) return;
    try {
      if (questions.has(message.requestID)) {
        const question = questions.get(message.requestID);
        const questionDirectory = question?.directory || directory;
        if (message.reply !== 'reject') {
          const answers = Array.isArray(message.answers)
            ? message.answers.map((answer) => (Array.isArray(answer) ? answer.map((value) => String(value).slice(0, 2000)) : []))
            : (question?.items || []).map((item) => [item?.options?.[0]?.label ?? '']);
          await api(`/question/${encodeURIComponent(message.requestID)}/reply`, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify({ answers }),
          }, questionDirectory);
        } else {
          await api(`/question/${encodeURIComponent(message.requestID)}/reject`, { method: 'POST' }, questionDirectory);
        }
        const remaining = await api('/question', {}, questionDirectory);
        if (Array.isArray(remaining) && remaining.some((item) => item?.id === message.requestID)) {
          throw new Error('OpenCode 仍在等待该回答，请确认项目目录路由');
        }
        questions.delete(message.requestID);
      } else if (client.permission?.reply) {
        await client.permission.reply({ requestID: message.requestID, reply: message.reply });
      } else if (client.postSessionIdPermissionsPermissionId && request.sessionID) {
        await client.postSessionIdPermissionsPermissionId({
          path: { id: request.sessionID, permissionID: message.requestID },
          body: { response: message.reply },
        });
      } else {
        await api(`/permission/${encodeURIComponent(message.requestID)}/reply`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ reply: message.reply }),
        });
      }
      permissions.delete(message.requestID);
      sendControl({ type: 'monitor.permission.resolved', requestID: message.requestID, reply: message.reply });
      log(`permission ${message.requestID} -> ${message.reply}`);
      closeControlIfIdle();
    } catch (e) {
      sendControl({ type: 'monitor.control.error', requestID: message.requestID, error: e.message });
      log(`permission reply failed: ${e.message}`);
    }
  }

  let reconcilingStatuses = false;
  async function reconcileStatuses() {
    if (reconcilingStatuses) return;
    reconcilingStatuses = true;
    try {
      const groups = new Map();
      for (const session of sessions.values()) {
        const projectDirectory = session.directory || directory;
        if (!groups.has(projectDirectory)) groups.set(projectDirectory, []);
        groups.get(projectDirectory).push(session);
      }
      await Promise.all([...groups].map(async ([projectDirectory, knownSessions]) => {
        try {
          const statuses = await api('/session/status', {}, projectDirectory);
          if (!statuses || typeof statuses !== 'object' || Array.isArray(statuses)) return;
          for (const session of knownSessions) {
            // Do not overwrite events received while this request was in flight.
            if (sessions.get(session.id) !== session) continue;
            const present = Object.hasOwn(statuses, session.id);
            const info = statuses[session.id];
            // OpenCode removes idle sessions from this sparse status map.
            // Preserve errors and other terminal states when a key is absent.
            const type = present ? info?.type : (['busy', 'retry'].includes(session.status) ? 'idle' : session.status);
            if (session.status === type || !['idle', 'busy', 'retry'].includes(type)) continue;
            updateSession(session.id, { status: type, retry: type === 'retry' ? safeValue(info) : null, tool: type === 'busy' ? session.tool : null });
            if (type === 'idle') publishOutput(session.id);
          }
        } catch {}
      }));
    } finally {
      reconcilingStatuses = false;
    }
  }

  async function reconcileQuestions() {
    try {
      const pending = await api('/question');
      if (!Array.isArray(pending)) return;
      const ids = new Set();
      for (const item of pending) {
        if (!item?.id) continue;
        ids.add(item.id);
        upsertQuestion(item, directory);
      }
      for (const id of [...questions.keys()]) {
        if (ids.has(id)) continue;
        questions.delete(id);
        permissions.delete(id);
        push({ type: 'monitor.permission.resolved', requestID: id });
      }
    } catch {}
  }

  ensureControlConnection();
  void loadInitialState();
  const heartbeatMs = Number(process.env.AGENT_LIGHT_HEARTBEAT_MS) || 15000;
  const heartbeat = setInterval(() => {
    ensureControlConnection();
    sendControl(hello('control'));
    void reconcileStatuses();
    void reconcileQuestions();
  }, heartbeatMs);
  heartbeat.unref?.();

  return {
    event: async ({ event }) => {
      try {
        switch (event.type) {
          case 'message.updated': {
            const info = event.properties?.info || event.properties?.message || {};
            if (info.role === 'user' && info.id && !seenUserMessages.has(info.id)) {
              seenUserMessages.add(info.id);
              if (lastMode !== 'busy' && lastMode !== 'thinking') sendStatus('thinking');
            }
            if (info.role === 'user' && info.id && info.sessionID) {
              userMessages.set(info.id, info.sessionID);
              publishInput(info.sessionID, info.id);
            }
            if (info.role === 'assistant') {
              if (info.sessionID && info.id) assistantMessages.set(info.sessionID, info.id);
              updateSession(info.sessionID, {
                agent: info.agent,
                model: info.modelID ? `${info.providerID}/${info.modelID}` : undefined,
                cost: info.cost,
                tokens: info.tokens,
                error: info.error ? safeValue(info.error) : null,
              });
            } else if (info.role === 'user') {
              updateSession(info.sessionID, {
                agent: info.agent,
                model: info.model ? `${info.model.providerID}/${info.model.modelID}` : undefined,
                status: 'busy',
              });
            }
            break;
          }
          case 'session.created':
          case 'session.updated':
            upsertSession(event.properties?.info);
            break;
          case 'session.deleted':
            if (sessions.get(event.properties?.sessionID)?.parentID) {
              sessions.delete(event.properties.sessionID);
              break;
            }
            sessions.delete(event.properties?.sessionID);
            push({ type: 'monitor.session.remove', sessionID: event.properties?.sessionID });
            break;
          case 'session.status': {
            const type = event.properties?.status?.type || 'idle';
            // Some OpenCode builds never emit session.idle; idle status also finalizes the answer.
            if (type === 'idle') publishOutput(event.properties?.sessionID);
            updateSession(event.properties?.sessionID, {
              status: type,
              ...(type === 'idle' ? { tool: null } : {}),
              retry: type === 'retry' ? safeValue(event.properties.status) : null,
            });
            break;
          }
          case 'todo.updated':
            updateSession(event.properties?.sessionID, { todos: safeValue(event.properties?.todos || []) });
            break;
          case 'message.part.updated': {
            const part = event.properties?.part;
            if (part?.type === 'text' && !part.synthetic && !part.ignored && part.id && part.sessionID && part.messageID) {
              messageParts.set(part.id, { sessionID: part.sessionID, messageID: part.messageID, text: String(part.text || '').slice(0, 12000) });
              publishInput(part.sessionID, part.messageID);
            } else if (part?.type === 'tool') {
              updateSession(part.sessionID, {
                status: part.state?.status === 'error' ? 'error' : 'busy',
                tool: {
                  callID: part.callID,
                  name: part.tool,
                  status: part.state?.status,
                  title: part.state?.title,
                  input: safeValue(part.state?.input),
                  output: safeValue(part.state?.output),
                  error: part.state?.error,
                  time: part.state?.time,
                },
              });
            }
            break;
          }
          case 'permission.asked':
          case 'permission.updated': {
            if (!event.properties?.id && !event.properties?.permissionID) break;
            const request = {
              ...safeValue(event.properties),
              id: event.properties.id || event.properties.permissionID,
              status: 'pending',
              createdAt: Date.now(),
            };
            permissions.set(request.id, request);
            push({ type: 'monitor.permission', request });
            sendStatus('alarm');
            ensureControlConnection();
            break;
          }
          case 'question.asked':
            // OpenCode broadcasts question events to every loaded plugin
            // context. Only the directory whose scoped API owns the request
            // may publish it or acknowledge a reply.
            await reconcileQuestions();
            ensureControlConnection();
            break;
          case 'question.replied':
          case 'question.rejected': {
            const requestID = event.properties?.requestID;
            if (!requestID) break;
            questions.delete(requestID);
            permissions.delete(requestID);
            push({ type: 'monitor.permission.resolved', requestID });
            sendStatus('busy');
            closeControlIfIdle();
            break;
          }
          case 'permission.replied':
            permissions.delete(event.properties?.requestID);
            sendControl({
              type: 'monitor.permission.resolved',
              requestID: event.properties?.requestID,
              reply: event.properties?.reply,
            });
            sendStatus('busy');
            closeControlIfIdle();
            break;
          case 'session.error':
            updateSession(event.properties?.sessionID, {
              status: 'error',
              error: safeValue(event.properties?.error),
            });
            sendStatus('error');
            break;
          case 'session.idle':
            publishOutput(event.properties?.sessionID);
            updateSession(event.properties?.sessionID, { status: 'idle', tool: null });
            sendStatus('success');
            break;
        }
      } catch (e) {
        log(`event handler error: ${e.message}`);
      }
    },
    'tool.execute.before': async (input, output) => {
      updateSession(input.sessionID, {
        status: 'busy',
        tool: {
          callID: input.callID,
          name: input.tool,
          status: 'running',
          input: safeValue(output.args),
          startedAt: Date.now(),
        },
      });
      sendStatus('busy');
    },
    'tool.execute.after': async (input, output) => {
      updateSession(input.sessionID, {
        status: 'busy',
        tool: {
          callID: input.callID,
          name: input.tool,
          status: 'completed',
          title: output.title,
          input: safeValue(input.args),
          output: safeValue(output.output),
          metadata: safeValue(output.metadata),
          endedAt: Date.now(),
        },
      });
    },
  };
};
