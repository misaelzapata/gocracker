import os
import time

import psycopg
from flask import Flask, jsonify, request
from psycopg.rows import dict_row


app = Flask(__name__)
DATABASE_URL = os.environ.get(
    "DATABASE_URL", "postgresql://todos:todos@postgres:5432/todos"
)


def get_conn():
    return psycopg.connect(DATABASE_URL, autocommit=True, row_factory=dict_row)


def init_db():
    deadline = time.time() + 60
    last_error = None
    while time.time() < deadline:
        try:
            with get_conn() as conn:
                with conn.cursor() as cur:
                    cur.execute(
                        """
                        create table if not exists todos (
                            id serial primary key,
                            title text not null,
                            done boolean not null default false,
                            created_at timestamptz not null default now()
                        )
                        """
                    )
            return
        except Exception as exc:
            last_error = exc
            time.sleep(1)
    raise RuntimeError(f"database did not become ready: {last_error}")


@app.get("/health")
def health():
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("select count(*) as total from todos")
            row = cur.fetchone()
    return jsonify(status="ok", database="postgres", total=row["total"])


@app.get("/")
def index():
    return """
<!doctype html>
<html lang="es">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>TODO + PostgreSQL</title>
    <style>
      :root {
        --bg: #f2efe8;
        --panel: #fffaf0;
        --ink: #1c1917;
        --accent: #0f766e;
        --accent-light: rgba(15,118,110,0.08);
        --muted: #57534e;
        --line: #d6d3d1;
        --red: #dc2626;
        --blue: #2563eb;
        --green: #16a34a;
      }
      * { box-sizing: border-box; margin: 0; }
      body {
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
        background:
          radial-gradient(circle at top right, rgba(15,118,110,0.15), transparent 30%),
          linear-gradient(180deg, #f7f4ed, var(--bg));
        color: var(--ink);
        min-height: 100vh;
      }
      .shell {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 20px;
        max-width: 960px;
        margin: 40px auto;
        padding: 0 16px;
      }
      @media (max-width: 700px) { .shell { grid-template-columns: 1fr; } }

      .card {
        background: var(--panel);
        border: 1px solid var(--line);
        border-radius: 20px;
        padding: 24px;
        box-shadow: 0 12px 32px rgba(28,25,23,0.06);
      }
      h1 { font-size: 1.6rem; margin-bottom: 4px; }
      .subtitle { color: var(--muted); font-size: 0.88rem; margin-bottom: 20px; }
      .stats { display: flex; gap: 10px; margin-bottom: 16px; flex-wrap: wrap; }
      .stat {
        font-size: 0.82rem;
        padding: 4px 12px;
        border-radius: 999px;
        background: var(--accent-light);
        color: var(--accent);
        font-weight: 600;
      }

      /* form */
      .add-form { display: flex; gap: 8px; margin-bottom: 20px; }
      .add-form input {
        flex: 1;
        border: 1px solid var(--line);
        border-radius: 999px;
        padding: 10px 16px;
        font: inherit;
        font-size: 0.92rem;
        background: #fff;
        outline: none;
        transition: border-color 0.15s;
      }
      .add-form input:focus { border-color: var(--accent); }
      .btn {
        border: none;
        border-radius: 999px;
        padding: 10px 18px;
        font: inherit;
        font-size: 0.88rem;
        font-weight: 600;
        cursor: pointer;
        transition: opacity 0.15s;
      }
      .btn:hover { opacity: 0.85; }
      .btn-primary { background: var(--accent); color: #fff; }

      /* list */
      .todo-list { display: grid; gap: 6px; }
      .todo-row {
        display: flex;
        align-items: center;
        gap: 10px;
        padding: 10px 14px;
        border: 1px solid var(--line);
        border-radius: 12px;
        background: rgba(255,255,255,0.7);
        cursor: pointer;
        transition: border-color 0.15s, box-shadow 0.15s;
      }
      .todo-row:hover { border-color: var(--accent); box-shadow: 0 2px 8px rgba(15,118,110,0.1); }
      .todo-row.active { border-color: var(--accent); background: var(--accent-light); }
      .todo-check {
        width: 20px; height: 20px;
        border: 2px solid var(--line);
        border-radius: 6px;
        flex-shrink: 0;
        display: grid; place-items: center;
        cursor: pointer;
        transition: all 0.15s;
        background: #fff;
      }
      .todo-check.checked { background: var(--accent); border-color: var(--accent); }
      .todo-check.checked::after { content: "\\2713"; color: #fff; font-size: 13px; font-weight: 700; }
      .todo-label { flex: 1; min-width: 0; font-size: 0.92rem; }
      .todo-label.done { text-decoration: line-through; color: var(--muted); }
      .todo-id { color: var(--muted); font-size: 0.78rem; flex-shrink: 0; }
      .empty-msg { color: var(--muted); font-style: italic; font-size: 0.9rem; padding: 12px 0; }

      /* detail panel */
      .detail-empty {
        display: flex;
        align-items: center;
        justify-content: center;
        color: var(--muted);
        font-size: 0.92rem;
        min-height: 200px;
      }
      .detail-header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
      .detail-header h2 { font-size: 1.1rem; }
      .badge {
        font-size: 0.75rem;
        padding: 3px 10px;
        border-radius: 999px;
        font-weight: 600;
      }
      .badge-open { background: #fef3c7; color: #92400e; }
      .badge-done { background: #d1fae5; color: #065f46; }
      .field { margin-bottom: 14px; }
      .field-label { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em; color: var(--muted); margin-bottom: 4px; }
      .field-value { font-size: 0.95rem; }
      .edit-input {
        width: 100%;
        border: 1px solid var(--line);
        border-radius: 10px;
        padding: 8px 12px;
        font: inherit;
        font-size: 0.92rem;
        outline: none;
        transition: border-color 0.15s;
      }
      .edit-input:focus { border-color: var(--accent); }
      .detail-actions { display: flex; gap: 8px; margin-top: 20px; padding-top: 16px; border-top: 1px solid var(--line); flex-wrap: wrap; }
      .btn-outline {
        background: #fff;
        border: 1px solid var(--line);
        border-radius: 999px;
        padding: 8px 16px;
        font: inherit;
        font-size: 0.84rem;
        font-weight: 600;
        cursor: pointer;
        transition: all 0.15s;
      }
      .btn-outline:hover { background: #fafaf9; }
      .btn-green { color: var(--green); border-color: var(--green); }
      .btn-green:hover { background: rgba(22,163,74,0.06); }
      .btn-blue { color: var(--blue); border-color: var(--blue); }
      .btn-blue:hover { background: rgba(37,99,235,0.06); }
      .btn-red { color: var(--red); border-color: var(--red); }
      .btn-red:hover { background: rgba(220,38,38,0.06); }

      /* inline confirm */
      .confirm-bar {
        display: flex;
        align-items: center;
        gap: 10px;
        margin-top: 12px;
        padding: 10px 14px;
        background: #fef2f2;
        border: 1px solid #fecaca;
        border-radius: 12px;
        font-size: 0.88rem;
      }
      .confirm-bar span { flex: 1; color: var(--red); font-weight: 500; }
      .btn-confirm-yes {
        background: var(--red); color: #fff; border: none;
        border-radius: 999px; padding: 6px 14px; font-size: 0.82rem;
        font-weight: 600; cursor: pointer;
      }
      .btn-confirm-no {
        background: #fff; color: var(--ink); border: 1px solid var(--line);
        border-radius: 999px; padding: 6px 14px; font-size: 0.82rem;
        font-weight: 600; cursor: pointer;
      }
      .hidden { display: none; }
    </style>
  </head>
  <body>
    <div class="shell">
      <!-- LEFT: list -->
      <div class="card">
        <h1>TODO + PostgreSQL</h1>
        <p class="subtitle">Tareas guardadas en PostgreSQL via Compose</p>
        <div class="stats" id="stats"></div>
        <form class="add-form" id="add-form">
          <input id="new-title" placeholder="Nueva tarea..." required>
          <button type="submit" class="btn btn-primary">Agregar</button>
        </form>
        <div class="todo-list" id="todo-list"></div>
      </div>

      <!-- RIGHT: detail -->
      <div class="card" id="detail-panel">
        <div class="detail-empty" id="detail-empty">Selecciona una tarea para ver detalles</div>
        <div class="hidden" id="detail-content">
          <div class="detail-header">
            <h2>Detalle</h2>
            <span class="badge" id="d-badge"></span>
          </div>
          <div class="field">
            <div class="field-label">ID</div>
            <div class="field-value" id="d-id"></div>
          </div>
          <div class="field">
            <div class="field-label">Titulo</div>
            <div class="field-value" id="d-title-view"></div>
            <div id="d-title-edit-wrap" style="display:none">
              <input class="edit-input" id="d-title-input">
              <div style="display:flex;gap:6px;margin-top:6px">
                <button class="btn-outline btn-blue" id="d-save-title" type="button">Guardar</button>
                <button class="btn-outline" id="d-cancel-title" type="button">Cancelar</button>
              </div>
            </div>
          </div>
          <div class="field">
            <div class="field-label">Estado</div>
            <div class="field-value" id="d-status"></div>
          </div>
          <div class="field">
            <div class="field-label">Creada</div>
            <div class="field-value" id="d-created"></div>
          </div>
          <div class="detail-actions">
            <button class="btn-outline btn-green" id="d-toggle" type="button"></button>
            <button class="btn-outline btn-blue" id="d-edit" type="button">Editar titulo</button>
            <button class="btn-outline btn-red" id="d-delete" type="button">Eliminar</button>
          </div>
          <div class="hidden" id="d-confirm"></div>
        </div>
      </div>
    </div>

    <script>
      let todos = [];
      let selectedId = null;

      const $ = id => document.getElementById(id);

      function fmtDate(iso) {
        const d = new Date(iso);
        const date = d.toLocaleDateString("es-ES", {year:"numeric", month:"long", day:"numeric"});
        const time = d.toLocaleTimeString("es-ES", {hour:"2-digit", minute:"2-digit", second:"2-digit"});
        return date + ", " + time;
      }

      function fmtRelative(iso) {
        const diff = Date.now() - new Date(iso).getTime();
        const mins = Math.floor(diff / 60000);
        if (mins < 1) return "hace un momento";
        if (mins < 60) return "hace " + mins + " min";
        const hrs = Math.floor(mins / 60);
        if (hrs < 24) return "hace " + hrs + "h";
        const days = Math.floor(hrs / 24);
        return "hace " + days + "d";
      }

      // ---- API ----
      async function apiList() {
        const r = await fetch("/api/todos");
        return r.json();
      }
      async function apiCreate(title) {
        await fetch("/api/todos", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({title})});
      }
      async function apiUpdate(id, data) {
        await fetch("/api/todos/"+id, {method:"PATCH", headers:{"Content-Type":"application/json"}, body:JSON.stringify(data)});
      }
      async function apiDelete(id) {
        await fetch("/api/todos/"+id, {method:"DELETE"});
      }

      // ---- RENDER ----
      async function reload() {
        todos = await apiList();
        renderList();
        renderDetail();
      }

      function renderList() {
        const list = $("todo-list");
        const stats = $("stats");
        const total = todos.length;
        const done = todos.filter(t=>t.done).length;
        stats.innerHTML = total
          ? '<span class="stat">Total: '+total+'</span><span class="stat">Pendientes: '+(total-done)+'</span><span class="stat">Completadas: '+done+'</span>'
          : '';
        if (!total) {
          list.innerHTML = '<div class="empty-msg">No hay tareas todavia</div>';
          return;
        }
        list.innerHTML = "";
        for (const t of todos) {
          const row = document.createElement("div");
          row.className = "todo-row" + (t.id === selectedId ? " active" : "");
          row.innerHTML =
            '<div class="todo-check'+(t.done?' checked':'')+'" data-id="'+t.id+'"></div>' +
            '<span class="todo-label'+(t.done?' done':'')+'">'+esc(t.title)+'</span>' +
            '<span class="todo-id">#'+t.id+' · '+fmtRelative(t.created_at)+'</span>';
          row.querySelector(".todo-check").addEventListener("click", async (e) => {
            e.stopPropagation();
            await apiUpdate(t.id, {done: !t.done});
            await reload();
          });
          row.addEventListener("click", () => { selectedId = t.id; renderList(); renderDetail(); });
          list.appendChild(row);
        }
      }

      function renderDetail() {
        const todo = todos.find(t => t.id === selectedId);
        if (!todo) {
          $("detail-empty").classList.remove("hidden");
          $("detail-content").classList.add("hidden");
          return;
        }
        $("detail-empty").classList.add("hidden");
        $("detail-content").classList.remove("hidden");

        $("d-id").textContent = "#" + todo.id;
        $("d-title-view").textContent = todo.title;
        $("d-title-view").style.display = "";
        $("d-title-edit-wrap").style.display = "none";
        $("d-status").textContent = todo.done ? "Completada" : "Pendiente";
        $("d-created").textContent = fmtDate(todo.created_at);

        const badge = $("d-badge");
        badge.textContent = todo.done ? "COMPLETADA" : "PENDIENTE";
        badge.className = "badge " + (todo.done ? "badge-done" : "badge-open");

        $("d-toggle").textContent = todo.done ? "Reabrir" : "Completar";
        $("d-confirm").classList.add("hidden");
        $("d-confirm").innerHTML = "";
      }

      function esc(s) {
        const d = document.createElement("div");
        d.textContent = s;
        return d.innerHTML;
      }

      // ---- ACTIONS ----
      $("add-form").addEventListener("submit", async (e) => {
        e.preventDefault();
        const inp = $("new-title");
        const title = inp.value.trim();
        if (!title) return;
        await apiCreate(title);
        inp.value = "";
        await reload();
      });

      $("d-toggle").addEventListener("click", async () => {
        const todo = todos.find(t => t.id === selectedId);
        if (!todo) return;
        await apiUpdate(todo.id, {done: !todo.done});
        await reload();
      });

      $("d-edit").addEventListener("click", () => {
        const todo = todos.find(t => t.id === selectedId);
        if (!todo) return;
        $("d-title-view").style.display = "none";
        $("d-title-edit-wrap").style.display = "";
        const inp = $("d-title-input");
        inp.value = todo.title;
        inp.focus();
        inp.select();
      });

      async function saveTitle() {
        const v = $("d-title-input").value.trim();
        if (!v || !selectedId) return;
        await apiUpdate(selectedId, {title: v});
        await reload();
      }
      $("d-save-title").addEventListener("click", saveTitle);
      $("d-title-input").addEventListener("keydown", (e) => {
        if (e.key === "Enter") { e.preventDefault(); saveTitle(); }
      });

      $("d-cancel-title").addEventListener("click", () => {
        $("d-title-view").style.display = "";
        $("d-title-edit-wrap").style.display = "none";
      });

      $("d-delete").addEventListener("click", () => {
        const box = $("d-confirm");
        box.classList.remove("hidden");
        box.innerHTML =
          '<div class="confirm-bar">' +
            '<span>Eliminar esta tarea?</span>' +
            '<button class="btn-confirm-yes" id="d-yes-delete" type="button">Si, eliminar</button>' +
            '<button class="btn-confirm-no" id="d-no-delete" type="button">No</button>' +
          '</div>';
        $("d-yes-delete").addEventListener("click", async () => {
          await apiDelete(selectedId);
          selectedId = null;
          await reload();
        });
        $("d-no-delete").addEventListener("click", () => {
          box.classList.add("hidden");
          box.innerHTML = "";
        });
      });

      // keyboard: Escape cancels edit
      document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") {
          $("d-title-view").style.display = "";
          $("d-title-edit-wrap").style.display = "none";
          const box = $("d-confirm");
          box.classList.add("hidden");
          box.innerHTML = "";
        }
      });

      reload();
    </script>
  </body>
</html>
"""


@app.get("/api/todos")
def list_todos():
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                select id, title, done, created_at
                from todos
                order by id asc
                """
            )
            rows = cur.fetchall()
    for r in rows:
        r["created_at"] = r["created_at"].isoformat()
    return jsonify(rows)


@app.post("/api/todos")
def create_todo():
    payload = request.get_json(silent=True) or {}
    title = str(payload.get("title", "")).strip()
    if not title:
        return jsonify(error="title is required"), 400
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                insert into todos (title)
                values (%s)
                returning id, title, done
                """,
                (title,),
            )
            row = cur.fetchone()
    return jsonify(row), 201


@app.patch("/api/todos/<int:todo_id>")
def update_todo(todo_id):
    payload = request.get_json(silent=True) or {}
    with get_conn() as conn:
        with conn.cursor() as cur:
            sets = []
            params = []
            if "title" in payload:
                title = str(payload["title"]).strip()
                if not title:
                    return jsonify(error="title cannot be empty"), 400
                sets.append("title = %s")
                params.append(title)
            if "done" in payload:
                sets.append("done = %s")
                params.append(bool(payload["done"]))
            if not sets:
                return jsonify(error="nothing to update"), 400
            params.append(todo_id)
            cur.execute(
                f"update todos set {', '.join(sets)} where id = %s "
                "returning id, title, done",
                params,
            )
            row = cur.fetchone()
    if not row:
        return jsonify(error="not found"), 404
    return jsonify(row)


@app.delete("/api/todos/<int:todo_id>")
def delete_todo(todo_id):
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                "delete from todos where id = %s returning id", (todo_id,)
            )
            row = cur.fetchone()
    if not row:
        return jsonify(error="not found"), 404
    return "", 204


if __name__ == "__main__":
    init_db()
    app.run(host="0.0.0.0", port=8080)
