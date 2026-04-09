const express = require("express");

const app = express();
app.use(express.json());

const items = [];

app.get("/", (_req, res) =>
  res.json({ app: "express-api", endpoints: ["/health", "/items"] })
);
app.get("/health", (_req, res) =>
  res.json({ status: "ok", count: items.length })
);
app.get("/items", (_req, res) => res.json({ items }));
app.post("/items", (req, res) => {
  const { name } = req.body || {};
  if (!name) return res.status(400).json({ error: "name required" });
  const item = { id: items.length + 1, name };
  items.push(item);
  res.status(201).json(item);
});

const port = parseInt(process.env.PORT || "3000", 10);
app.listen(port, "0.0.0.0", () =>
  console.log(`express-api listening on :${port}`)
);
