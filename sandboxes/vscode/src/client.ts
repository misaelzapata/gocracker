export interface Sandbox {
  id: string;
  state: string;
  image: string;
  uds_path: string;
  guest_ip: string;
  created_at: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exit_code: number;
  wall_ms: number;
}

export interface ExecOptions {
  env?: string[];
  workdir?: string;
  stdin?: string;
  timeoutMs?: number;
}

export class GocrackrClient {
  constructor(private readonly baseUrl: string) {}

  async healthz(): Promise<boolean> {
    const res = await fetch(`${this.baseUrl}/healthz`);
    return res.ok;
  }

  async leaseSandbox(templateId: string, timeoutMs?: number): Promise<Sandbox> {
    const body: Record<string, unknown> = { template_id: templateId };
    if (timeoutMs !== undefined) {
      // sandboxd expects timeout in nanoseconds
      body["timeout"] = timeoutMs * 1_000_000;
    }
    const res = await fetch(`${this.baseUrl}/sandboxes/lease`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`leaseSandbox: HTTP ${res.status}: ${text}`);
    }
    const data = (await res.json()) as { sandbox: Sandbox };
    return data.sandbox;
  }

  async exec(
    sandboxId: string,
    cmd: string[],
    opts?: ExecOptions
  ): Promise<ExecResult> {
    const body: Record<string, unknown> = { cmd };
    if (opts?.env !== undefined) {
      body["env"] = opts.env;
    }
    if (opts?.workdir !== undefined) {
      body["workdir"] = opts.workdir;
    }
    if (opts?.stdin !== undefined) {
      body["stdin"] = opts.stdin;
    }
    if (opts?.timeoutMs !== undefined) {
      body["timeout_ms"] = opts.timeoutMs;
    }
    const res = await fetch(`${this.baseUrl}/sandboxes/${sandboxId}/exec`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`exec: HTTP ${res.status}: ${text}`);
    }
    return (await res.json()) as ExecResult;
  }

  async uploadFile(
    sandboxId: string,
    guestPath: string,
    content: string
  ): Promise<void> {
    // Ensure the path segment passed to the URL doesn't start with '/'
    // since the route pattern is /sandboxes/{id}/files/{path...}
    const normalizedPath = guestPath.startsWith("/")
      ? guestPath.slice(1)
      : guestPath;
    const res = await fetch(
      `${this.baseUrl}/sandboxes/${sandboxId}/files/${normalizedPath}`,
      {
        method: "PUT",
        headers: { "Content-Type": "text/plain" },
        body: content,
      }
    );
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`uploadFile: HTTP ${res.status}: ${text}`);
    }
  }

  async deleteSandbox(id: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/sandboxes/${id}`, {
      method: "DELETE",
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`deleteSandbox: HTTP ${res.status}: ${text}`);
    }
  }

  async listSandboxes(): Promise<Sandbox[]> {
    const res = await fetch(`${this.baseUrl}/sandboxes`);
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`listSandboxes: HTTP ${res.status}: ${text}`);
    }
    const data = (await res.json()) as { sandboxes: Sandbox[] };
    return data.sandboxes;
  }

  async recycleSandbox(id: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/sandboxes/${id}/recycle`, { method: 'POST' });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`recycleSandbox: HTTP ${res.status}: ${text}`);
    }
  }
}
