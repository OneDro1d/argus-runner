import type { FastifyInstance } from "fastify";

export default function strategiesRoutes(app: FastifyInstance) {
  app.get("/api/v1/strategies/:strategyPairId", async () => ({ ok: true }));
}
