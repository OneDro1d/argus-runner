import Fastify from "fastify";

// Real defect: these are TEST FIXTURES inside a test file, never real routes of the SUT — a repo
// scan must never propose scenarios out of them (found for real against
// packages/website-api/src/middleware/idempotency.test.ts).
function buildTestApp() {
  const app = Fastify();
  app.get("/x", async () => ({ n: 1 }));
  app.get("/probe", async () => ({ ok: true }));
  app.get("/protected", async () => ({ ok: true }));
  app.get("/whoami", async () => ({ ok: true }));
  app.post("/echo", async () => ({ ok: true }));
  app.post("/fail", async () => {
    throw new Error("fail");
  });
  return app;
}

module.exports = { buildTestApp };
