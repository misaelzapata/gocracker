import * as vscode from 'vscode';

export interface GocrackrConfig {
  kernelPath: string;
  sandboxdUrl: string;
  networkMode: string;
  defaultMemMb: number;
  autoStartDaemon: boolean;
  keepSandboxOnError: boolean;
}

export function getConfig(): GocrackrConfig {
  const cfg = vscode.workspace.getConfiguration('gocracker');
  return {
    kernelPath:         cfg.get<string>('kernelPath', ''),
    sandboxdUrl:        cfg.get<string>('sandboxdUrl', 'http://127.0.0.1:9091'),
    networkMode:        cfg.get<string>('networkMode', 'slirp'),
    defaultMemMb:       cfg.get<number>('defaultMemMb', 256),
    autoStartDaemon:    cfg.get<boolean>('autoStartDaemon', true),
    keepSandboxOnError: cfg.get<boolean>('keepSandboxOnError', false),
  };
}
