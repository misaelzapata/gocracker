import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';

export const SUPPORTED_EXTENSIONS: string[] = ['.js', '.mjs', '.ts', '.py', '.go'];

/**
 * Returns true if a bun lockfile exists in any workspace folder root.
 */
function hasBunLockfile(): boolean {
  const folders = vscode.workspace.workspaceFolders;
  if (!folders || folders.length === 0) {
    return false;
  }
  for (const folder of folders) {
    const root = folder.uri.fsPath;
    if (
      fs.existsSync(path.join(root, 'bun.lockb')) ||
      fs.existsSync(path.join(root, 'bun.lock'))
    ) {
      return true;
    }
  }
  return false;
}

/**
 * Maps a VS Code TextDocument to a gocracker template ID.
 * Returns undefined for unsupported file types.
 */
export function templateForDocument(doc: vscode.TextDocument): string | undefined {
  const ext = path.extname(doc.fileName).toLowerCase();

  switch (ext) {
    case '.js':
    case '.mjs':
      return 'base-node';

    case '.ts':
      return hasBunLockfile() ? 'base-bun' : 'base-node';

    case '.py':
      return 'base-python';

    case '.go':
      return 'base-go';

    default:
      return undefined;
  }
}

/**
 * Returns the command array used to execute a file inside a sandbox for the
 * given template.
 */
export function execCommandForTemplate(template: string, filePath: string): string[] {
  switch (template) {
    case 'base-node':
      return ['node', filePath];

    case 'base-bun':
      return ['bun', 'run', filePath];

    case 'base-python':
      return ['python3', filePath];

    case 'base-go':
      return ['go', 'run', filePath];

    default:
      throw new Error(`Unknown gocracker template: ${template}`);
  }
}
