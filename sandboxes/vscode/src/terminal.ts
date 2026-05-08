import * as vscode from 'vscode';
import { GocrackrClient } from './client';

export function openSandboxShell(sandboxId: string, client: GocrackrClient): void {
  const writeEmitter = new vscode.EventEmitter<string>();
  const closeEmitter = new vscode.EventEmitter<number>();

  let inputBuf = '';
  let cwd = '/';
  const PROMPT = () => `\r\n\x1b[32m[gocracker:${sandboxId.slice(0, 8)}]\x1b[0m ${cwd} $ `;

  const pty: vscode.Pseudoterminal = {
    onDidWrite: writeEmitter.event,
    onDidClose: closeEmitter.event,
    open() {
      writeEmitter.fire(`gocracker sandbox shell — ${sandboxId}\r\nType commands and press Enter. Type "exit" to close.\r\n`);
      writeEmitter.fire(PROMPT());
    },
    close() {
      writeEmitter.dispose();
      closeEmitter.dispose();
    },
    handleInput(data: string) {
      // data is a single character or escape sequence from the terminal
      if (data === '\r') {                    // Enter
        const line = inputBuf.trim();
        inputBuf = '';
        writeEmitter.fire('\r\n');
        if (line === 'exit' || line === 'quit') {
          writeEmitter.fire('Closing shell...\r\n');
          closeEmitter.fire(0);
          return;
        }
        if (!line) {
          writeEmitter.fire(PROMPT());
          return;
        }
        // Run the command
        const cmd = ['/bin/sh', '-c', line];
        client.exec(sandboxId, cmd, { workdir: cwd, timeoutMs: 30000 })
          .then(result => {
            if (result.stdout) {
              writeEmitter.fire(result.stdout.replace(/\n/g, '\r\n'));
            }
            if (result.stderr) {
              writeEmitter.fire('\x1b[31m' + result.stderr.replace(/\n/g, '\r\n') + '\x1b[0m');
            }
            if (result.exit_code !== 0) {
              writeEmitter.fire(`\x1b[33m[exit ${result.exit_code}]\x1b[0m`);
            }
            writeEmitter.fire(PROMPT());
          })
          .catch((err: Error) => {
            writeEmitter.fire(`\x1b[31merror: ${err.message}\x1b[0m`);
            writeEmitter.fire(PROMPT());
          });
      } else if (data === '\x7f' || data === '\b') {  // Backspace
        if (inputBuf.length > 0) {
          inputBuf = inputBuf.slice(0, -1);
          writeEmitter.fire('\b \b');          // erase last char on screen
        }
      } else if (data.startsWith('\x1b')) {    // Escape sequences (arrow keys etc.) — ignore
        // do nothing
      } else {
        inputBuf += data;
        writeEmitter.fire(data);               // echo the character
      }
    },
  };

  const terminal = vscode.window.createTerminal({
    name: `gocracker: ${sandboxId.slice(0, 12)}`,
    pty,
  });
  terminal.show();
}
