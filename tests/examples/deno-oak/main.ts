import { Application, Router } from "https://deno.land/x/oak@v17.1.3/mod.ts";

const app = new Application();
const router = new Router();

let counter = 0;

router
  .get("/", (ctx) => {
    ctx.response.body = { app: "deno-oak", endpoints: ["/health", "/inc"] };
  })
  .get("/health", (ctx) => {
    ctx.response.body = { status: "ok", counter };
  })
  .post("/inc", (ctx) => {
    counter++;
    ctx.response.body = { counter };
  });

app.use(router.routes());
app.use(router.allowedMethods());

const port = parseInt(Deno.env.get("PORT") ?? "8000", 10);
console.log(`deno-oak listening on :${port}`);
await app.listen({ port });
