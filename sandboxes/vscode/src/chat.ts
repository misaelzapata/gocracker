import * as vscode from 'vscode';
import { GocrackrClient } from './client';

// Maps file extension / language hint in fenced code block to template ID
const LANG_TO_TEMPLATE: Record<string, string> = {
  py:         'base-python',
  python:     'base-python',
  js:         'base-node',
  javascript: 'base-node',
  ts:         'base-node',
  typescript: 'base-node',
  go:         'base-go',
};

const LANG_TO_EXT: Record<string, string> = {
  py:         '.py',
  python:     '.py',
  js:         '.js',
  javascript: '.js',
  ts:         '.ts',
  typescript: '.ts',
  go:         '.go',
};

const LANG_TO_CMD: Record<string, (p: string) => string[]> = {
  py:         p => ['python3', p],
  python:     p => ['python3', p],
  js:         p => ['node', p],
  javascript: p => ['node', p],
  ts:         p => ['node', p],
  typescript: p => ['node', '--input-type=module', p],
  go:         p => ['go', 'run', p],
};

/** Extracts the first fenced code block from a chat message. Returns {lang, code} or null. */
function extractCodeBlock(text: string): { lang: string; code: string } | null {
  const match = text.match(/```(\w*)\n([\s\S]*?)```/);
  if (!match) return null;
  return { lang: match[1].toLowerCase() || 'js', code: match[2] };
}

/** Returns a fallback template by scanning the message for language keywords. */
function detectLangFromText(text: string): string | null {
  const lower = text.toLowerCase();
  if (lower.includes('python') || lower.includes('.py')) return 'python';
  if (lower.includes('go ') || lower.includes('golang')) return 'go';
  if (lower.includes('typescript') || lower.includes('.ts')) return 'typescript';
  if (lower.includes('javascript') || lower.includes('.js') || lower.includes('node')) return 'js';
  return null;
}

export function registerChatParticipant(
  context: vscode.ExtensionContext,
  client: GocrackrClient,
): vscode.Disposable {
  // vscode.chat is available from VS Code 1.90+; guard against older versions
  if (!('chat' in vscode)) {
    return { dispose() {} };
  }

  const handler: vscode.ChatRequestHandler = async (
    request: vscode.ChatRequest,
    _chatContext: vscode.ChatContext,
    stream: vscode.ChatResponseStream,
    token: vscode.CancellationToken,
  ) => {
    const text = request.prompt;

    // Try to find a code block
    let block = extractCodeBlock(text);
    if (!block) {
      // No code block — check if user is asking for help
      if (text.trim().toLowerCase().startsWith('help') || text.trim() === '?') {
        stream.markdown(
          'I can run code in isolated micro-VM sandboxes.\n\n' +
          'Wrap your code in a fenced block:\n' +
          '````\n```python\nprint("hello")\n```\n````\n\n' +
          'Supported languages: Python, JavaScript/TypeScript (Node), Go.'
        );
        return;
      }

      // Try to detect language from text and use a placeholder
      const lang = detectLangFromText(text) ?? 'js';
      // Ask user to provide code
      stream.markdown(
        `No code block found. Please wrap your code in a fenced block like:\n` +
        `\`\`\`\`\n\`\`\`${lang}\n// your code here\n\`\`\`\n\`\`\`\``
      );
      return;
    }

    const template = LANG_TO_TEMPLATE[block.lang] ?? 'base-node';
    const ext      = LANG_TO_EXT[block.lang]      ?? '.js';
    const cmdFn    = LANG_TO_CMD[block.lang]       ?? ((p: string) => ['node', p]);
    const guestPath = `/tmp/gocracker_chat${ext}`;

    stream.progress('Leasing sandbox...');

    if (token.isCancellationRequested) return;

    let sbId: string | undefined;
    try {
      const sb = await client.leaseSandbox(template);
      sbId = sb.id;

      stream.progress('Uploading code...');
      await client.uploadFile(sbId, guestPath, block.code);

      if (token.isCancellationRequested) {
        await client.deleteSandbox(sbId).catch(() => undefined);
        return;
      }

      stream.progress('Running...');
      const result = await client.exec(sbId, cmdFn(guestPath), { timeoutMs: 30000 });

      // Format result
      const output = [
        result.stdout ? '**stdout**\n```\n' + result.stdout.trim() + '\n```' : '',
        result.stderr ? '**stderr**\n```\n' + result.stderr.trim() + '\n```' : '',
        result.exit_code !== 0 ? `\n> exit code: ${result.exit_code}` : '',
        `\n_wall time: ${result.wall_ms} ms_`,
      ].filter(Boolean).join('\n\n');

      stream.markdown(output || '_No output_');
    } catch (err: any) {
      stream.markdown(`**Error**: ${err.message ?? String(err)}`);
    } finally {
      if (sbId) {
        await client.deleteSandbox(sbId).catch(() => undefined);
      }
    }
  };

  const participant = (vscode.chat as any).createChatParticipant('gocracker.run', handler);
  participant.iconPath = new vscode.ThemeIcon('vm');

  return participant;
}
