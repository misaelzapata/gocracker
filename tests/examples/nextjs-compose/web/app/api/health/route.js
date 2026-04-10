import { Pool } from "pg";

const pool = new Pool({
  connectionString:
    process.env.DATABASE_URL ||
    "postgresql://nextjs:nextjs@db:5432/nextjs",
  max: 4,
});

export async function GET() {
  try {
    const res = await pool.query("SELECT now() AS now, current_database() AS db");
    return Response.json({
      status: "ok",
      db: res.rows[0].db,
      now: res.rows[0].now,
    });
  } catch (e) {
    return Response.json({ status: "error", error: String(e) }, { status: 500 });
  }
}
