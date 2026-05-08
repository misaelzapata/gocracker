// Minimal vscode API mock for unit tests (no VS Code process required)
export const window = {
  showErrorMessage: jest.fn(),
  showWarningMessage: jest.fn(),
  showInformationMessage: jest.fn(),
  createOutputChannel: jest.fn(),
  withProgress: jest.fn(),
  createTerminal: jest.fn(),
};
export const commands = { executeCommand: jest.fn(), registerCommand: jest.fn() };
export const workspace = { onDidChangeConfiguration: jest.fn() };
export const ProgressLocation = { Notification: 15 };
export const StatusBarAlignment = { Right: 2 };
export const ThemeIcon = class { constructor(public id: string) {} };
export const EventEmitter = class {
  event = jest.fn();
  fire = jest.fn();
  dispose = jest.fn();
};
export const Uri = { file: (p: string) => ({ fsPath: p }) };
export const chat = { createChatParticipant: jest.fn(() => ({ iconPath: null, dispose: jest.fn() })) };
