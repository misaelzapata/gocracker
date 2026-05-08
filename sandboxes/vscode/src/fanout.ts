import * as vscode from 'vscode';
import { GocrackrClient } from './client';
import { GocrackrOutputPanel } from './panel';
import { templateForDocument, execCommandForTemplate } from './language';

function guestExt(doc: vscode.TextDocument): string {
  const map: Record<string, string> = { python: '.py', javascript: '.js', typescript: '.ts', go: '.go' };
  return map[doc.languageId] ?? (require('path').extname(doc.fileName) || '.tmp');
}

export async function runFanOut(
  client: GocrackrClient,
  source: string,
  doc: vscode.TextDocument,
  context: vscode.ExtensionContext,
  n: number,
): Promise<void> {
  const panel = GocrackrOutputPanel.getOrCreate(context);
  const template = templateForDocument(doc);
  if (!template) {
    vscode.window.showErrorMessage('gocracker: Unsupported file type for fan-out');
    return;
  }

  panel.clear();
  panel.show();
  panel.append(`Fan-out: running ${n} parallel sandboxes (template: ${template})...\n\n`);

  const ext = guestExt(doc);
  const guestPath = `/tmp/gocracker_fanout${ext}`;

  const start = Date.now();

  // Run N slots concurrently
  const slots = await Promise.allSettled(
    Array.from({ length: n }, async (_, i) => {
      const sb = await client.leaseSandbox(template);
      try {
        await client.uploadFile(sb.id, guestPath, source);
        const cmd = execCommandForTemplate(template, guestPath);
        const result = await client.exec(sb.id, cmd, { timeoutMs: 30000 });
        return { slot: i + 1, result, sbId: sb.id };
      } finally {
        await client.deleteSandbox(sb.id).catch(() => undefined);
      }
    })
  );

  const wallMs = Date.now() - start;

  // Collect outputs
  const outputs: { slot: number; stdout: string; stderr: string; exitCode: number }[] = [];
  for (const s of slots) {
    if (s.status === 'fulfilled') {
      const { slot, result } = s.value;
      outputs.push({ slot, stdout: result.stdout, stderr: result.stderr, exitCode: result.exit_code });
    } else {
      panel.appendError(`Slot failed: ${s.reason}\n`);
    }
  }

  // Check if all stdout outputs are identical
  const allSame = outputs.length > 1 && outputs.every(o => o.stdout === outputs[0].stdout);

  if (allSame) {
    panel.append(`All ${outputs.length} runs produced identical output:\n\n`);
    panel.append(outputs[0].stdout);
    if (outputs[0].stderr) panel.appendError(outputs[0].stderr);
    if (outputs[0].exitCode !== 0) panel.appendError(`\n[exit ${outputs[0].exitCode}]\n`);
  } else {
    for (const o of outputs) {
      panel.append(`── Slot ${o.slot} ${'─'.repeat(50 - String(o.slot).length)}\n`);
      if (o.stdout) panel.append(o.stdout);
      if (o.stderr) panel.appendError(o.stderr);
      if (o.exitCode !== 0) panel.appendError(`[exit ${o.exitCode}]\n`);
      panel.append('\n');
    }
  }

  panel.append(`\n✓ fan-out complete — ${outputs.length}/${n} succeeded, wall time ${wallMs} ms\n`);
}
