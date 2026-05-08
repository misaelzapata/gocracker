import * as vscode from 'vscode';

const ANSI_COLOR_MAP: Record<number, string> = {
  0:  'inherit',   // reset — handled specially below
  30: '#000000',   // black
  31: '#cc0000',   // red
  32: '#4e9a06',   // green
  33: '#c4a000',   // yellow
  34: '#3465a4',   // blue
  35: '#75507b',   // magenta
  36: '#06989a',   // cyan
  37: '#d3d7cf',   // white
  90: '#555753',   // bright black
  91: '#ef2929',   // bright red
  92: '#8ae234',   // bright green
  93: '#fce94f',   // bright yellow
  94: '#729fcf',   // bright blue
  95: '#ad7fa8',   // bright magenta
  96: '#34e2e2',   // bright cyan
  97: '#eeeeec',   // bright white
};

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

function ansiToHtml(text: string): string {
  // First escape HTML entities, then convert ANSI codes
  const escaped = escapeHtml(text);
  let result = '';
  let openSpan = false;

  // Match ANSI escape sequences of the form ESC [ <n> m
  const ansiRe = /\x1b\[(\d+)m/g;
  let lastIndex = 0;
  let match: RegExpExecArray | null;

  while ((match = ansiRe.exec(escaped)) !== null) {
    // Append text before this escape code
    result += escaped.slice(lastIndex, match.index);
    lastIndex = ansiRe.lastIndex;

    const code = parseInt(match[1], 10);

    if (code === 0) {
      // Reset
      if (openSpan) {
        result += '</span>';
        openSpan = false;
      }
    } else if (ANSI_COLOR_MAP[code] !== undefined) {
      if (openSpan) {
        result += '</span>';
      }
      result += `<span style="color:${ANSI_COLOR_MAP[code]}">`;
      openSpan = true;
    }
    // Unknown codes are silently dropped
  }

  // Append remaining text
  result += escaped.slice(lastIndex);
  if (openSpan) {
    result += '</span>';
  }

  return result;
}

function buildHtml(content: string): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>gocracker Output</title>
<style>
  body {
    margin: 0;
    padding: 8px;
    background: #1e1e1e;
    color: #ffffff;
    font-family: monospace;
  }
  pre#out {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
</head>
<body>
<pre id="out">${ansiToHtml(content)}</pre>
</body>
</html>`;
}

export class GocrackrOutputPanel implements vscode.Disposable {
  private static instance: GocrackrOutputPanel | undefined;
  private readonly panel: vscode.WebviewPanel;
  private content: string = '';

  private constructor(context: vscode.ExtensionContext) {
    this.panel = vscode.window.createWebviewPanel(
      'gocrackr.output',
      'gocracker Output',
      vscode.ViewColumn.Beside,
      {
        enableScripts: false,
        retainContextWhenHidden: true,
      }
    );

    this.panel.webview.html = buildHtml(this.content);

    this.panel.onDidDispose(
      () => {
        GocrackrOutputPanel.instance = undefined;
      },
      null,
      context.subscriptions
    );
  }

  /** Get or create the singleton panel. */
  static getOrCreate(context: vscode.ExtensionContext): GocrackrOutputPanel {
    if (!GocrackrOutputPanel.instance) {
      GocrackrOutputPanel.instance = new GocrackrOutputPanel(context);
    }
    return GocrackrOutputPanel.instance;
  }

  append(text: string): void {
    this.content += text;
    this.panel.webview.html = buildHtml(this.content);
  }

  appendError(text: string): void {
    this.content += `\x1b[31m${text}\x1b[0m`;
    this.panel.webview.html = buildHtml(this.content);
  }

  clear(): void {
    this.content = '';
    this.panel.webview.html = buildHtml(this.content);
  }

  show(): void {
    this.panel.reveal(vscode.ViewColumn.Beside, true);
  }

  dispose(): void {
    this.panel.dispose();
  }
}
