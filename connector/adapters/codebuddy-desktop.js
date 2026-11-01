// AgentTower CodeBuddy CN desktop bridge, running in the IDE extension host.
const vscode = require('vscode');
const path = require('path');
const crypto = require('crypto');
const WebSocket = require(path.join(vscode.env.appRoot, 'node_modules', 'ws'));
const command = 'tencentcloud.codingcopilot.chat.sendMessage';
let stop;

function activate(context) {
  const folder = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!folder) return;
  const directory = path.resolve(folder);
  const hash = crypto.createHash('md5').update(directory).digest('hex');
  const id = `codebuddy-bridge:${hash}`;
  const raw = process.env.AGENT_LIGHT_URL || 'ws://127.0.0.1:17654';
  const url = new URL(raw);
  if (!['ws:', 'wss:'].includes(url.protocol) || !['127.0.0.1', 'localhost'].includes(url.hostname)) return;
  let socket, retry, heartbeat, closed = false, queue = Promise.resolve();
  const results = new Map();
  const send = message => {
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ ...message, instanceId: id }));
  };
  async function handle(message) {
    const result = { type: 'monitor.action.result', targetInstanceId: message.targetInstanceId, actionID: message.actionID, action: message.action, sessionID: message.sessionID, ok: false };
    try {
      if (message.action !== 'prompt' || !/^[0-9a-f]{32}$/i.test(message.sessionID) || typeof message.text !== 'string' || !message.text.trim() || message.text.length > 4000) throw Error('Invalid CodeBuddy request');
      const receipt = await vscode.commands.executeCommand(command, {
        message: message.text, options: { conversationId: message.sessionID, headless: false, prefillOnly: false }
      });
      if (!receipt || receipt.error || receipt.id !== message.sessionID) throw Error(receipt?.error || 'CodeBuddy did not accept the message');
      result.ok = true;
      result.disposition = 'submitted'; // Not proof that inference has started.
    } catch (error) { result.error = String(error.message || error); }
    return result;
  }
  function connect() {
    if (closed) return;
    socket = new WebSocket(raw, { handshakeTimeout: 3000, maxPayload: 1024 * 1024 });
    socket.on('open', async () => {
      try {
        const commands = await vscode.commands.getCommands(true);
        if (!commands.includes(command)) { socket.close(); return; }
        send({ type: 'monitor.hello', connection: 'control', instance: {
          id, agent: 'codebuddy-bridge', directory, contextId: hash,
          capabilities: { sessionControl: false, permission: false }
        } });
        heartbeat = setInterval(() => send({ type: 'monitor.heartbeat' }), 5000);
      } catch { socket.close(); }
    });
    socket.on('message', data => {
      let message;
      try { message = JSON.parse(data.toString()); } catch { return; }
      if (message.type !== 'control.session' || message.contextId !== hash || typeof message.actionID !== 'string' || !message.actionID) return;
      const fingerprint = JSON.stringify([message.action, message.sessionID, message.text]);
      const old = results.get(message.actionID);
      if (old) {
        if (old.fingerprint === fingerprint) old.promise.then(send);
        return;
      }
      const promise = queue.then(() => handle(message));
      results.set(message.actionID, { fingerprint, promise });
      queue = promise.then(send).catch(() => {});
      // Bound completed receipt retention; preserve queued/in-flight requests.
      promise.finally(() => { if (results.size > 256) results.delete(results.keys().next().value); });
    });
    socket.on('error', () => {});
    socket.on('close', () => {
      clearInterval(heartbeat);
      if (!closed) retry = setTimeout(connect, 2000);
    });
  }
  stop = () => { closed = true; clearTimeout(retry); clearInterval(heartbeat); socket?.close(); };
  context.subscriptions.push({ dispose: stop });
  connect();
}
function deactivate() { stop?.(); }
module.exports = { activate, deactivate };
