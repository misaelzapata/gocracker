async function getStatus() {
  try {
    const res = await fetch(`http://localhost:3000/api/health`, { cache: "no-store" });
    return await res.json();
  } catch (e) {
    return { status: "unknown", error: String(e) };
  }
}

export default async function Page() {
  const status = await getStatus();
  return (
    <main style={{ fontFamily: "system-ui", padding: 32 }}>
      <h1>nextjs-compose</h1>
      <p>gocracker test fixture: Next.js 14 + Postgres via compose.</p>
      <pre>{JSON.stringify(status, null, 2)}</pre>
    </main>
  );
}
