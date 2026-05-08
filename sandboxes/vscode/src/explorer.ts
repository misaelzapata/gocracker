import * as vscode from 'vscode';
import { GocrackrClient, Sandbox } from './client';

export class SandboxItem extends vscode.TreeItem {
  constructor(public readonly sandbox: Sandbox) {
    super(sandbox.id.slice(0, 12), vscode.TreeItemCollapsibleState.None);
    this.description = `${sandbox.state} · ${sandbox.image}`;
    this.tooltip = `ID: ${sandbox.id}\nState: ${sandbox.state}\nIP: ${sandbox.guest_ip}`;
    this.contextValue = 'sandbox';
    this.iconPath = new vscode.ThemeIcon(
      sandbox.state === 'running' ? 'vm-running' : 'vm'
    );
  }
}

export class SandboxExplorer
  implements vscode.TreeDataProvider<SandboxItem>, vscode.Disposable
{
  private readonly _onDidChangeTreeData =
    new vscode.EventEmitter<SandboxItem | undefined | null | void>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private sandboxes: Sandbox[] = [];
  private timer: NodeJS.Timeout | undefined;

  constructor(private readonly client: GocrackrClient) {}

  /** Start polling /sandboxes every 3 seconds. */
  startPolling(): void {
    if (this.timer) {
      return;
    }
    this.timer = setInterval(async () => {
      try {
        this.sandboxes = await this.client.listSandboxes();
        this._onDidChangeTreeData.fire();
      } catch {
        // sandboxd may not be running — ignore
      }
    }, 3000);
  }

  /** Fire a manual refresh immediately. */
  refresh(): void {
    this.client.listSandboxes().then(sandboxes => {
      this.sandboxes = sandboxes;
      this._onDidChangeTreeData.fire();
    }).catch(() => {
      // sandboxd may not be running — ignore
    });
  }

  getTreeItem(element: SandboxItem): vscode.TreeItem {
    return element;
  }

  async getChildren(): Promise<SandboxItem[]> {
    return this.sandboxes.map(s => new SandboxItem(s));
  }

  dispose(): void {
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    this._onDidChangeTreeData.dispose();
  }
}
