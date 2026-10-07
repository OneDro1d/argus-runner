const app = require("express")();

// Allowed under money_handling: probe/health/metrics-shaped GETs.
app.get("/healthz", function (req, res) { res.json({ ok: true }); });
app.get("/metrics", function (req, res) { res.send("# metrics"); });
app.get("/status", function (req, res) { res.json({ ok: true }); });
app.get("/health/status", function (req, res) { res.json({ ok: true }); });
app.get("/info", function (req, res) { res.json({ version: "1.0" }); });

// NOT allowed under money_handling: user-scoped or deep/writey-shaped GETs.
app.get("/kyc/status", function (req, res) { res.json({ verified: true }); });
app.get("/api/v1/user/info", function (req, res) { res.json({}); });
app.get("/api/v1/user/deposit-address/:chainId", function (req, res) { res.json({}); });
app.get("/api/admin/users", function (req, res) { res.json([]); });

module.exports = app;
