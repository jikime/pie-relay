#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { createInterface } from 'node:readline';
import { handleWorkspaceRequest } from '/opt/pie-relay/node-executor/workspace.mjs';

const output = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);
const sessionId = `pie-e2e-session-${process.pid}`;
const permissions = new Map();

output({ type: 'ready' });
const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });

for await (const line of lines) {
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    output({ type: 'error', message: 'invalid json request' });
    continue;
  }
  if (request.type === 'workspace') {
    output(handleWorkspaceRequest(request));
  } else if (request.type === 'chat') {
    const prompt = String(request.prompt || request.text || '');
    output({ type: 'session_id', sessionId: request.sessionId || sessionId });
    if (prompt === 'slow-manager-recovery') {
      await new Promise((resolvePromise) => setTimeout(resolvePromise, 3000));
    }
    if (prompt === 'request-permission') {
      const requestId = `permission-${Date.now()}`;
      permissions.set(requestId, prompt);
      output({ type: 'permission_request', requestId, toolName: 'E2ETool', input: { value: 'safe' } });
      continue;
    }
    if (prompt === 'image-attachment') {
      const images = Array.isArray(request.images) ? request.images : [];
      if (images.length !== 1 || images[0]?.mimeType !== 'image/png' || typeof images[0]?.data !== 'string') {
        output({ type: 'error', message: 'image attachment was not delivered intact' });
        continue;
      }
      const image = Buffer.from(images[0].data, 'base64');
      const digest = createHash('sha256').update(image).digest('hex').slice(0, 16);
      output({ type: 'text', text: `pie-e2e:image-attachment:${images.length}:${images[0].mimeType}:${digest}` });
      output({ type: 'done', sessionId: request.sessionId || sessionId });
      continue;
    }
    if (prompt === 'report-working-directory') {
      output({ type: 'text', text: `pie-e2e:cwd:${process.env.CLI_RELAY_DEFAULT_CWD || ''}` });
      output({ type: 'done', sessionId: request.sessionId || sessionId });
      continue;
    }
    output({ type: 'text', text: `pie-e2e:${prompt}` });
    output({ type: 'done', sessionId: request.sessionId || sessionId });
  } else if (request.type === 'permission_response') {
    const pending = permissions.get(request.requestId);
    if (!pending) {
      output({ type: 'error', message: 'unknown permission request' });
      continue;
    }
    permissions.delete(request.requestId);
    output({ type: 'text', text: request.allow ? 'pie-e2e:permission-allowed' : 'pie-e2e:permission-denied' });
    output({ type: 'done', sessionId });
  } else if (request.type === 'abort') {
    output({ type: 'aborted' });
  }
}
