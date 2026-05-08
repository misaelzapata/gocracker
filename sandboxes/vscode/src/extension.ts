import * as path from 'path';
import * as vscode from 'vscode';
import { getConfig } from './config';
import { GocrackrClient } from './client';
import { DaemonManager } from './daemon';
import { templateForDocument, execCommandForTemplate } from './language';
import { GocrackrOutputPanel } from './panel';
import { SandboxExplorer, SandboxItem } from './explorer';
import { openSandboxShell } from './terminal';
import { downloadKernel }  from './daemon';
import { runFanOut } from './fanout';
import { registerChatParticipant } from './chat';

let daemonManager: DaemonManager | undefined;

/** Derive a guest file extension from the document's language ID or filename. */
function guestExtension(doc: vscode.TextDocument): string {
  const fromLang: Record<string, string> = {
    python:     '.py',
    javascript: '.js',
    typescript: '.ts',
    go:         '.go',
  };
  if (fromLang[doc.languageId]) {
    return fromLang[doc.languageId];
  }
  const ext = path.extname(doc.fileName);
  return ext || '.tmp';
}

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  if (process.platform !== 'linux') {
    vscode.window.showWarningMessage(
      'gocracker requires Linux with KVM. It will not function on this platform.'
    );
    return;
  }

  // 1. Output channel for daemon logs
  const outputChannel = vscode.window.createOutputChannel('gocracker');

  // 2. Client
  const client = new GocrackrClient(getConfig().sandboxdUrl);

  // 3. Daemon manager
  const daemon = new DaemonManager(context, outputChannel);
  daemonManager = daemon;

  // 4. Sandbox explorer
  const explorer = new SandboxExplorer(client);
  const treeView = vscode.window.registerTreeDataProvider('gocrackrSandboxes', explorer);
  explorer.startPolling();

  // 5. Chat participant
  const chatParticipant = registerChatParticipant(context, client);
  context.subscriptions.push(chatParticipant);

  // 6. Status bar item
  const statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  statusBar.command = 'gocracker.startDaemon';
  statusBar.show();

  async function updateStatusBar(): Promise<void> {
    const running = await daemon.isRunning(getConfig().sandboxdUrl);
    statusBar.text    = running ? '$(vm) gocracker: ready' : '$(vm-outline) gocracker: stopped';
    statusBar.tooltip = 'gocracker sandbox control plane';
  }

  updateStatusBar();

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration(e => {
      if (e.affectsConfiguration('gocracker')) {
        updateStatusBar();
      }
    }),
  );

  // ─── Commands ───────────────────────────────────────────────────────────────

  async function runCode(source: string, doc: vscode.TextDocument): Promise<void> {
    const panel = GocrackrOutputPanel.getOrCreate(context);

    await vscode.window.withProgress(
      { location: vscode.ProgressLocation.Notification, title: 'gocracker', cancellable: false },
      async () => {
        const config = getConfig();

        if (config.autoStartDaemon) {
          await daemon.ensure(config);
          updateStatusBar();
        }

        const template = templateForDocument(doc);
        if (!template) {
          vscode.window.showErrorMessage('gocracker: Unsupported file type');
          return;
        }

        panel.clear();
        panel.show();

        let sb: { id: string; state: string; image: string; uds_path: string; guest_ip: string; created_at: string } | undefined;

        try {
          panel.append('Leasing sandbox...\n');
          sb = await client.leaseSandbox(template);

          const ext      = guestExtension(doc);
          const guestPath = `/tmp/gocracker_run${ext}`;

          await client.uploadFile(sb.id, guestPath, source);

          panel.append('Running...\n');
          const cmd    = execCommandForTemplate(template, guestPath);
          const result = await client.exec(sb.id, cmd, { timeoutMs: 30000 });

          if (result.stdout) {
            panel.append(result.stdout);
          }
          if (result.stderr) {
            panel.appendError(result.stderr);
          }
          if (result.exit_code !== 0) {
            panel.appendError(`\nProcess exited with code ${result.exit_code}\n`);
          }

          panel.append(`\n✓ done in ${result.wall_ms} ms\n`);

          if (!config.keepSandboxOnError || result.exit_code === 0) {
            await client.deleteSandbox(sb.id);
          }
        } catch (err: any) {
          panel.appendError(err.message ?? String(err));
          if (sb) {
            if (!config.keepSandboxOnError) {
              await client.deleteSandbox(sb.id).catch(() => undefined);
            }
          }
        }
      },
    );
  }

  const cmdRunSelection = vscode.commands.registerCommand('gocracker.runSelection', async () => {
    const editor = vscode.window.activeTextEditor;
    if (!editor || editor.selection.isEmpty) {
      vscode.window.showErrorMessage('gocracker: No text selected');
      return;
    }
    const source = editor.document.getText(editor.selection);
    await runCode(source, editor.document);
  });

  const cmdRunFile = vscode.commands.registerCommand('gocracker.runFile', async () => {
    const editor = vscode.window.activeTextEditor;
    if (!editor) {
      vscode.window.showErrorMessage('gocracker: No active editor');
      return;
    }
    const source = editor.document.getText();
    await runCode(source, editor.document);
  });

  const cmdStartDaemon = vscode.commands.registerCommand('gocracker.startDaemon', async () => {
    try {
      await daemon.ensure(getConfig());
      updateStatusBar();
      vscode.window.showInformationMessage('gocracker daemon started');
    } catch (err: any) {
      vscode.window.showErrorMessage(`gocracker: ${err.message ?? err}`);
    }
  });

  const cmdStopDaemon = vscode.commands.registerCommand('gocracker.stopDaemon', async () => {
    await daemon.stop();
    updateStatusBar();
    vscode.window.showInformationMessage('gocracker daemon stopped');
  });

  const cmdSetupMcp = vscode.commands.registerCommand('gocracker.setupMcp', () => {
    const terminal = vscode.window.createTerminal('gocracker setup');
    terminal.show();
    terminal.sendText('gocracker-mcp setup');
  });

  const cmdRefreshExplorer = vscode.commands.registerCommand('gocracker.refreshExplorer', () => {
    explorer.refresh();
  });

  const cmdFanOut = vscode.commands.registerCommand('gocracker.fanOut', async () => {
    const editor = vscode.window.activeTextEditor;
    if (!editor || editor.selection.isEmpty) {
      vscode.window.showErrorMessage('gocracker: Select code to fan-out');
      return;
    }
    const nStr = await vscode.window.showInputBox({
      prompt: 'Number of parallel sandboxes',
      value: '3',
      validateInput: v => {
        const n = parseInt(v, 10);
        return (Number.isInteger(n) && n >= 2 && n <= 10) ? null : 'Enter a number between 2 and 10';
      },
    });
    if (!nStr) return;
    const n = parseInt(nStr, 10);

    const config = getConfig();
    if (config.autoStartDaemon) {
      await daemon.ensure(config);
      updateStatusBar();
    }

    const source = editor.document.getText(editor.selection);
    await runFanOut(client, source, editor.document, context, n);
  });

  const cmdDeleteSandbox = vscode.commands.registerCommand('gocracker.deleteSandbox', async (item?: SandboxItem) => {
    const id = item?.sandbox.id;
    if (!id) return;
    const confirm = await vscode.window.showWarningMessage(
      `Delete sandbox ${id.slice(0, 12)}?`, { modal: true }, 'Delete'
    );
    if (confirm !== 'Delete') return;
    try {
      await client.deleteSandbox(id);
      explorer.refresh();
    } catch (err: any) {
      vscode.window.showErrorMessage(`gocracker: ${err.message}`);
    }
  });

  const cmdRecycleSandbox = vscode.commands.registerCommand('gocracker.recycleSandbox', async (item?: SandboxItem) => {
    const id = item?.sandbox.id;
    if (!id) return;
    try {
      await client.recycleSandbox(id);
      explorer.refresh();
      vscode.window.showInformationMessage('Sandbox recycled');
    } catch (err: any) {
      vscode.window.showErrorMessage(`gocracker: ${err.message}`);
    }
  });

  const cmdExecShell = vscode.commands.registerCommand('gocracker.execShell', (item?: SandboxItem) => {
    const id = item?.sandbox.id;
    if (!id) {
      vscode.window.showErrorMessage('gocracker: right-click a sandbox in the explorer to open a shell');
      return;
    }
    openSandboxShell(id, client);
  });

  const cmdDownloadKernel = vscode.commands.registerCommand('gocracker.downloadKernel', async () => {
    try {
      const kernelPath = await downloadKernel(context, outputChannel);
      vscode.window.showInformationMessage(`gocracker: kernel saved to ${kernelPath}`);
    } catch (err: any) {
      vscode.window.showErrorMessage(`gocracker: kernel download failed — ${err.message}`);
    }
  });

  // ─── Push disposables ───────────────────────────────────────────────────────
  context.subscriptions.push(
    outputChannel,
    daemon,
    explorer,
    treeView,
    statusBar,
    cmdRunSelection,
    cmdRunFile,
    cmdStartDaemon,
    cmdStopDaemon,
    cmdSetupMcp,
    cmdRefreshExplorer,
    cmdFanOut,
    cmdDeleteSandbox,
    cmdRecycleSandbox,
    cmdExecShell,
    cmdDownloadKernel,
  );
}

export function deactivate(): Promise<void> | undefined {
  return daemonManager?.stop();
}
