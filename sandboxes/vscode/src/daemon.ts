import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as path from 'path';
import { GocrackrConfig } from './config';

const fs = require('fs') as typeof import('fs');

function delay(ms: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, ms));
}

export class DaemonManager implements vscode.Disposable {
  private proc: cp.ChildProcess | undefined;
  private readonly log: vscode.OutputChannel;

  constructor(
    private readonly context: vscode.ExtensionContext,
    log: vscode.OutputChannel
  ) {
    this.log = log;
  }

  /** Returns true if /healthz responds 200. Pure HTTP check, no process state. */
  async isRunning(sandboxdUrl: string): Promise<boolean> {
    try {
      const res = await fetch(`${sandboxdUrl}/healthz`);
      return res.status === 200;
    } catch {
      return false;
    }
  }

  /**
   * If not running, discovers kernel and spawns:
   *   gocracker-sandboxd serve
   *     --addr 127.0.0.1:9091
   *     --state-dir <globalStorageUri>/sandboxd-state
   *     --kernel-path <kernelPath>
   *     --network-mode <networkMode>
   *     --uds-group <currentUser>
   *
   * Polls /healthz every 200ms for up to 15s. Throws if it never comes up.
   */
  async ensure(config: GocrackrConfig): Promise<void> {
    if (await this.isRunning(config.sandboxdUrl)) {
      this.log.appendLine('[daemon] already running');
      return;
    }

    const kernelPath = this.discoverKernel(config);
    if (!kernelPath) {
      throw new Error(
        'gocracker kernel not found. Set gocracker.kernelPath or GOCRACKER_KERNEL.'
      );
    }
    this.log.appendLine(`[daemon] using kernel: ${kernelPath}`);

    const stateDir = path.join(this.context.globalStorageUri.fsPath, 'sandboxd-state');
    const udsGroup = process.env.USER || process.env.USERNAME || '';

    // Parse addr from URL (strip protocol)
    const url = new URL(config.sandboxdUrl);
    const addr = `${url.hostname}:${url.port || '9091'}`;

    const args = [
      'serve',
      '--addr', addr,
      '--state-dir', stateDir,
      '--kernel-path', kernelPath,
      '--network-mode', config.networkMode,
      '--uds-group', udsGroup,
    ];

    this.log.appendLine(`[daemon] spawning: gocracker-sandboxd ${args.join(' ')}`);

    const proc = cp.spawn('gocracker-sandboxd', args, {
      detached: false,
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    proc.stdout?.on('data', (data: Buffer) => {
      this.log.append(`[daemon stdout] ${data.toString()}`);
    });

    proc.stderr?.on('data', (data: Buffer) => {
      this.log.append(`[daemon stderr] ${data.toString()}`);
    });

    proc.on('error', (err: Error) => {
      this.log.appendLine(`[daemon] process error: ${err.message}`);
    });

    proc.on('exit', (code: number | null, signal: string | null) => {
      this.log.appendLine(`[daemon] process exited: code=${code} signal=${signal}`);
      if (this.proc === proc) {
        this.proc = undefined;
      }
    });

    this.proc = proc;

    // Poll /healthz every 200ms for up to 15s
    const maxAttempts = 75; // 75 * 200ms = 15000ms
    for (let i = 0; i < maxAttempts; i++) {
      await delay(200);
      if (await this.isRunning(config.sandboxdUrl)) {
        this.log.appendLine('[daemon] sandboxd is up');
        return;
      }
    }

    // Timed out — kill the process and throw
    proc.kill();
    this.proc = undefined;
    throw new Error('gocracker-sandboxd failed to start within 15 seconds');
  }

  /** Kills the child process started by ensure(). */
  async stop(): Promise<void> {
    if (this.proc) {
      this.log.appendLine('[daemon] stopping sandboxd');
      this.proc.kill();
      this.proc = undefined;
    }
  }

  dispose(): void {
    this.stop();
  }

  private discoverKernel(config: GocrackrConfig): string | undefined {
    // 1. config.kernelPath if non-empty
    if (config.kernelPath && fs.existsSync(config.kernelPath)) {
      return config.kernelPath;
    }

    // 2. GOCRACKER_KERNEL env var
    const envKernel = process.env.GOCRACKER_KERNEL;
    if (envKernel && fs.existsSync(envKernel)) {
      return envKernel;
    }

    // 3. globalStorageUri/kernels/gocracker-guest-standard-vmlinux
    const storageKernel = path.join(
      this.context.globalStorageUri.fsPath,
      'kernels',
      'gocracker-guest-standard-vmlinux'
    );
    if (fs.existsSync(storageKernel)) {
      return storageKernel;
    }

    // 4. Relative to gocracker-sandboxd binary location
    try {
      const result = cp.spawnSync('which', ['gocracker-sandboxd'], { encoding: 'utf8' });
      if (result.status === 0 && result.stdout.trim()) {
        const binPath = result.stdout.trim();
        const relativeKernel = path.join(
          path.dirname(binPath),
          '..',
          'artifacts',
          'kernels',
          'gocracker-guest-standard-vmlinux'
        );
        const resolved = path.resolve(relativeKernel);
        if (fs.existsSync(resolved)) {
          return resolved;
        }
      }
    } catch {
      // which not available or failed — ignore
    }

    return undefined;
  }
}
