import { Hono } from "hono";

const app = new Hono();

let counter = 0;

app.get("/", (c) =>
  c.json({ app: "bun-hono", endpoints: ["/health", "/inc"] })
);
app.get("/health", (c) =>
  c.json({ status: "ok", counter })
);
app.post("/inc", (c) => {
  counter++;
  return c.json({ counter });
});

const port = parseInt(process.env.PORT || "3000", 10);
console.log(`bun-hono listening on :${port}`);

export default { fetch: app.fetch, port };
